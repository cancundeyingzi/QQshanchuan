// qfile-link can download files from public QQ File Transfer
// (qfile.qq.com) share links.
package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/md5" // #nosec G501 -- exposed only as a user-requested compatibility checksum.
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- QQ's HMAC protocol and the requested compatibility checksum use SHA-1.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

const (
	qfileHost        = "qfile.qq.com"
	rpcBase          = "https://qfile.qq.com/http2rpc/gotrpc/noauth/"
	listService      = "trpc.file.FileFlashTrans/GetFileList"
	batchService     = "trpc.qqntv2.richmedia.InnerProxy/BatchDownload"
	listOIDB         = "0x93d4_1"
	batchOIDB        = "0x9248_4"
	protocolHMACKey  = "9EB18BB9ED457684"
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	listPageSize      = 100
	downloadBatch     = 30
	maxPageBytes      = 8 << 20
	maxRPCBytes       = 8 << 20
	copyBufferSize    = 256 << 10
	maxRedirects      = 10
	defaultFileMode   = 0o644
	defaultFolderMode = 0o755
)

var (
	filesetIDPattern   = regexp.MustCompile(`\\?"fileset_id\\?"\s*:\s*\\?"([0-9a-fA-F-]{36})\\?"`)
	pastedSharePattern = regexp.MustCompile(`(?i)(?:https?://)?qfile\.qq\.com/q/[a-z0-9_-]+`)
	volumePartPattern  = regexp.MustCompile(`(?i)^(.*)\.(\d{3,})$`)
)

type output struct {
	ShareURL    string       `json:"share_url"`
	FilesetID   string       `json:"fileset_id"`
	DownloadAPI string       `json:"download_api"`
	GeneratedAt time.Time    `json:"generated_at"`
	Files       []outputFile `json:"files"`
}

type outputFile struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Size        string `json:"size_bytes,omitempty"`
	DownloadURL string `json:"download_url,omitempty"`
	Error       string `json:"error,omitempty"`
}

type parsedShare struct {
	ShareURL  string
	FilesetID string
	Client    *qfileClient
	Files     []shareFile
}

type shareFile struct {
	Name       string
	Path       string
	Size       string
	PhysicalID string
	Error      string
}

type qfileClient struct {
	httpClient     *http.Client
	downloadClient *http.Client
	deviceID       string
	shareURL       string
}

type folderTask struct {
	parentID string
	path     string
}

type directDownload struct {
	URL   string
	Error string
}

type rpcEnvelope struct {
	RetCode json.RawMessage `json:"retcode"`
	Message string          `json:"message"`
	Error   struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	} `json:"error"`
	Data json.RawMessage `json:"data"`
}

type listRequest struct {
	FilesetID           string            `json:"fileset_id"`
	RequestInfos        []listRequestInfo `json:"req_infos"`
	SupportFolderStatus bool              `json:"support_folder_status"`
	SceneType           int               `json:"scene_type"`
}

type listRequestInfo struct {
	ParentID        string          `json:"parent_id"`
	RequestDepth    int             `json:"req_depth"`
	Count           int             `json:"count"`
	PaginationInfo  *string         `json:"pagination_info,omitempty"`
	FilterCondition filterCondition `json:"filter_condition"`
	SortConditions  []sortCondition `json:"sort_conditions"`
}

type filterCondition struct {
	FileCategory   int  `json:"file_category"`
	ExcludeDeleted bool `json:"exclude_deleted"`
}

type sortCondition struct {
	SortField int `json:"sort_field"`
	SortOrder int `json:"sort_order"`
}

type listResponse struct {
	FileLists []remoteFileList `json:"file_lists"`
}

type remoteFileList struct {
	ParentID       string       `json:"parent_id"`
	PaginationInfo string       `json:"pagination_info"`
	IsEnd          bool         `json:"is_end"`
	FileList       []remoteFile `json:"file_list"`
}

type remoteFile struct {
	CLIFileID  string `json:"cli_fileid"`
	Name       string `json:"name"`
	FileSize   string `json:"file_size"`
	IsDir      bool   `json:"is_dir"`
	SafeStatus int    `json:"safe_status"`
	Physical   struct {
		ID                  string `json:"id"`
		DownloadLimitStatus int    `json:"download_limit_status"`
	} `json:"physical"`
}

type batchDownloadRequest struct {
	RequestHeader struct {
		Agent int `json:"agent"`
	} `json:"req_head"`
	DownloadInfo []batchDownloadInfo `json:"download_info"`
	SceneType    int                 `json:"scene_type"`
}

type batchDownloadInfo struct {
	BatchID string `json:"batch_id"`
	Scene   struct {
		BusinessType int `json:"business_type"`
		AppType      int `json:"app_type"`
		SceneType    int `json:"scene_type"`
	} `json:"scene"`
	IndexNode struct {
		FileUUID string `json:"file_uuid"`
	} `json:"index_node"`
	URLType       int `json:"url_type"`
	DownloadScene int `json:"download_scene"`
}

type batchDownloadResponse struct {
	DownloadResponses []batchDownloadResult `json:"download_rsp"`
}

type batchDownloadResult struct {
	BatchID string `json:"batch_id"`
	RetCode string `json:"ret_code"`
	RetMsg  string `json:"ret_msg"`
	URL     string `json:"url"`
}

type archiveKind string

const (
	archiveUnknown archiveKind = ""
	archiveTar     archiveKind = "tar"
	archiveTarGzip archiveKind = "tar+gzip"
	archiveTarXZ   archiveKind = "tar+xz"
	archiveTarZstd archiveKind = "tar+zstd"
	archiveGzip    archiveKind = "gzip"
	archiveXZ      archiveKind = "xz"
	archiveZstd    archiveKind = "zstd"
)

type volumePart struct {
	path   string
	number int64
}

type volumeGroup struct {
	basePath string
	parts    []string
	kind     archiveKind
}

type archiveReport struct {
	found     int
	completed int
	extracted int
	failures  int
	files     []string
}

type fileHashValues struct {
	size   int64
	md5    string
	sha1   string
	sha256 string
}

func main() {
	var (
		asText  bool
		outPath string
		timeout time.Duration
	)

	flag.BoolVar(&asText, "text", false, "print tab-separated path and direct URL instead of JSON")
	flag.StringVar(&outPath, "output", "", "write direct-URL output to this file instead of stdout")
	flag.DurationVar(&timeout, "timeout", 45*time.Second, "timeout for each QQ API request")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage:\n  %s                              # interactive downloader\n  %s [options] <https://qfile.qq.com/q/SHARE_CODE>\n\n", os.Args[0], os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	// Launching the executable without arguments is the normal end-user flow.
	if flag.NArg() == 0 && !asText && outPath == "" {
		if err := runInteractive(timeout); err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			os.Exit(1)
		}
		return
	}
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	// Keep the original direct-URL extraction mode for scripts and integrations.
	ctx, cancel := context.WithTimeout(context.Background(), timeout*200)
	defer cancel()
	result, err := extract(ctx, flag.Arg(0), timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}

	var writer io.Writer = os.Stdout
	var closer io.Closer
	if outPath != "" {
		file, createErr := os.Create(outPath)
		if createErr != nil {
			fmt.Fprintln(os.Stderr, "Error creating output file:", createErr)
			os.Exit(1)
		}
		writer, closer = file, file
	}
	if closer != nil {
		defer closer.Close()
	}

	if asText {
		writeText(writer, result)
		return
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "Error writing output:", err)
		os.Exit(1)
	}
}

func runInteractive(timeout time.Duration) error {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("QQ 闪传下载器")
	fmt.Print("请粘贴 QQ 闪传链接，然后按 Enter：")
	rawURL, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("读取链接: %w", err)
	}
	if strings.TrimSpace(rawURL) == "" {
		return nil
	}
	shareURL, err := normalizePastedShareURL(rawURL)
	if err != nil {
		return err
	}

	fmt.Println("正在解析分享链接…")
	share, err := parseShare(context.Background(), shareURL, timeout)
	if err != nil {
		return err
	}
	available := 0
	for _, file := range share.Files {
		if file.Error == "" {
			available++
		}
	}
	fmt.Printf("解析完成：共 %d 个文件，其中 %d 个可下载。\n", len(share.Files), available)
	if available < len(share.Files) {
		fmt.Printf("有 %d 个文件被 QQ 限制或不可用，下载时会跳过并显示原因。\n", len(share.Files)-available)
	}

	continueFlow, err := waitForEnter(reader, "按 Enter 开始逐个下载文件：")
	if err != nil || !continueFlow {
		return err
	}

	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("获取当前运行目录: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("解析当前运行目录: %w", err)
	}
	downloaded, failures := downloadAll(context.Background(), share, root)
	if failures == 0 {
		fmt.Printf("下载完成：成功保存 %d 个文件到 %s。\n", len(downloaded), root)
	} else {
		fmt.Printf("下载阶段完成：成功 %d 个，失败或跳过 %d 个；文件保存在 %s。\n", len(downloaded), failures, root)
	}

	continueFlow, err = waitForEnter(reader, "按 Enter 检查已下载的 tar/gz/xz/zstd 压缩包及分卷并自动解压：")
	if err != nil || !continueFlow {
		return err
	}
	report := processVolumeArchives(downloaded, root)
	if report.found == 0 {
		fmt.Println("未发现可处理的 tar/gz/xz/zstd 压缩包或分卷。")
	} else if report.failures == 0 {
		fmt.Printf("压缩包处理完成：已处理 %d 组，解压得到 %d 个文件。原始压缩包和分卷已保留。\n", report.completed, report.extracted)
	} else {
		fmt.Printf("压缩包处理完成：成功 %d/%d 组，解压得到 %d 个文件，失败 %d 组。原始压缩包和分卷已保留。\n", report.completed, report.found, report.extracted, report.failures)
	}

	hashPaths := append([]string{}, downloaded...)
	hashPaths = append(hashPaths, report.files...)
	displayFileHashes(root, hashPaths)

	_, err = waitForEnter(reader, "哈希已展示。按 Enter 退出：")
	return err
}

func waitForEnter(reader *bufio.Reader, prompt string) (bool, error) {
	fmt.Print(prompt)
	line, err := reader.ReadString('\n')
	if errors.Is(err, io.EOF) {
		return len(line) > 0, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取输入: %w", err)
	}
	return true, nil
}

func normalizePastedShareURL(raw string) (string, error) {
	match := pastedSharePattern.FindString(strings.TrimSpace(raw))
	if match == "" {
		return "", errors.New("没有找到 QQ 闪传链接；请粘贴形如 https://qfile.qq.com/q/SHARE_CODE 的链接")
	}
	if !strings.Contains(match, "://") {
		match = "https://" + match
	}
	return validateShareURL(match)
}

func extract(ctx context.Context, rawShareURL string, timeout time.Duration) (output, error) {
	share, err := parseShare(ctx, rawShareURL, timeout)
	if err != nil {
		return output{}, err
	}
	result := output{
		ShareURL:    share.ShareURL,
		FilesetID:   share.FilesetID,
		DownloadAPI: rpcBase + batchService,
		GeneratedAt: time.Now().UTC(),
		Files:       make([]outputFile, 0, len(share.Files)),
	}

	physicalIDs := make([]string, 0, len(share.Files))
	for _, file := range share.Files {
		result.Files = append(result.Files, outputFile{
			Name:  file.Name,
			Path:  file.Path,
			Size:  file.Size,
			Error: file.Error,
		})
		if file.Error == "" {
			physicalIDs = append(physicalIDs, file.PhysicalID)
		}
	}

	directURLs := share.Client.resolveDownloads(ctx, physicalIDs)
	for index, file := range share.Files {
		if result.Files[index].Error != "" {
			continue
		}
		direct, ok := directURLs[file.PhysicalID]
		if !ok {
			result.Files[index].Error = "QQ omitted this file from its direct-download response"
			continue
		}
		if direct.Error != "" {
			result.Files[index].Error = direct.Error
			continue
		}
		result.Files[index].DownloadURL = withFilename(direct.URL, file.Name)
	}
	return result, nil
}

func parseShare(ctx context.Context, rawShareURL string, timeout time.Duration) (*parsedShare, error) {
	shareURL, err := validateShareURL(rawShareURL)
	if err != nil {
		return nil, err
	}
	client, err := newQFileClient(timeout, shareURL)
	if err != nil {
		return nil, err
	}
	filesetID, err := client.openShare(ctx)
	if err != nil {
		return nil, err
	}
	files, err := client.collectFiles(ctx, filesetID)
	if err != nil {
		return nil, err
	}
	return &parsedShare{
		ShareURL:  shareURL,
		FilesetID: filesetID,
		Client:    client,
		Files:     files,
	}, nil
}

func newQFileClient(timeout time.Duration, shareURL string) (*qfileClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}
	apiClient := &http.Client{
		Timeout: timeout,
		Jar:     jar,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if !strings.EqualFold(req.URL.Hostname(), qfileHost) {
				return fmt.Errorf("refusing redirect to non-QQ File Transfer host %q", req.URL.Hostname())
			}
			return nil
		},
	}
	downloadClient := &http.Client{
		// A file transfer can be large, so it intentionally has no whole-download
		// deadline. Reuse QQ's cookie jar because some download nodes require the
		// device cookie obtained from the share page. Redirects remain HTTP(S).
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too many download redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("refusing download redirect using %q", req.URL.Scheme)
			}
			return nil
		},
	}
	return &qfileClient{httpClient: apiClient, downloadClient: downloadClient, shareURL: shareURL}, nil
}

func validateShareURL(raw string) (string, error) {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid share URL: %w", err)
	}
	if parsed.User != nil || !strings.EqualFold(parsed.Hostname(), qfileHost) {
		return "", errors.New("only public qfile.qq.com share links are supported")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", errors.New("share URL must use HTTP or HTTPS")
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) != 2 || parts[0] != "q" || parts[1] == "" {
		return "", errors.New("expected a link in the form https://qfile.qq.com/q/SHARE_CODE")
	}
	parsed.Scheme = "https"
	parsed.Fragment = ""
	return parsed.String(), nil
}

func (c *qfileClient) openShare(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.shareURL, nil)
	if err != nil {
		return "", fmt.Errorf("create share-page request: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("open share page: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("open share page: server returned %s", resp.Status)
	}
	page, err := readLimited(resp.Body, maxPageBytes)
	if err != nil {
		return "", fmt.Errorf("read share page: %w", err)
	}

	match := filesetIDPattern.FindSubmatch(page)
	if len(match) != 2 {
		return "", errors.New("could not find the file-set id; the share may be expired or QQ changed its page format")
	}
	c.deviceID = findCookie(c.httpClient.Jar.Cookies(resp.Request.URL), "deviceId")
	if c.deviceID == "" {
		return "", errors.New("QQ did not issue the required deviceId cookie")
	}
	return string(match[1]), nil
}

func (c *qfileClient) collectFiles(ctx context.Context, filesetID string) ([]shareFile, error) {
	tasks := []folderTask{{}}
	visited := map[string]bool{"": true}
	files := make([]shareFile, 0)

	for taskIndex := 0; taskIndex < len(tasks); taskIndex++ {
		task := tasks[taskIndex]
		pagination := ""
		for {
			fileList, err := c.getFileList(ctx, filesetID, task.parentID, pagination)
			if err != nil {
				return nil, fmt.Errorf("list %q: %w", printablePath(task.path), err)
			}

			for _, file := range fileList.FileList {
				filePath := joinPath(task.path, file.Name)
				if file.IsDir {
					if file.CLIFileID != "" && !visited[file.CLIFileID] {
						visited[file.CLIFileID] = true
						tasks = append(tasks, folderTask{parentID: file.CLIFileID, path: filePath})
					}
					continue
				}

				entry := shareFile{
					Name:       file.Name,
					Path:       filePath,
					Size:       file.FileSize,
					PhysicalID: file.Physical.ID,
				}
				switch {
				case file.SafeStatus == 4:
					entry.Error = "QQ marks this file as unavailable after its safety review"
				case file.Physical.DownloadLimitStatus == 1 || file.Physical.DownloadLimitStatus == 2:
					entry.Error = "QQ restricts this file to its own client; no public-browser download URL is available"
				case file.Physical.ID == "":
					entry.Error = "QQ did not provide a downloadable file id"
				}
				files = append(files, entry)
			}

			if fileList.IsEnd {
				break
			}
			if fileList.PaginationInfo == "" || fileList.PaginationInfo == pagination {
				return nil, fmt.Errorf("list %q: QQ returned an invalid pagination cursor", printablePath(task.path))
			}
			pagination = fileList.PaginationInfo
		}
	}
	return files, nil
}

func (c *qfileClient) getFileList(ctx context.Context, filesetID, parentID, pagination string) (remoteFileList, error) {
	var cursor *string
	if pagination != "" {
		cursor = &pagination
	}
	payload := listRequest{
		FilesetID: filesetID,
		RequestInfos: []listRequestInfo{{
			ParentID:       parentID,
			RequestDepth:   1,
			Count:          listPageSize,
			PaginationInfo: cursor,
			FilterCondition: filterCondition{
				FileCategory:   0,
				ExcludeDeleted: true,
			},
			SortConditions: []sortCondition{
				{SortField: 4, SortOrder: 2},
				{SortField: 0, SortOrder: 1},
			},
		}},
		SupportFolderStatus: true,
		SceneType:           103,
	}
	var response listResponse
	if err := c.postRPC(ctx, listService, listOIDB, payload, &response); err != nil {
		return remoteFileList{}, err
	}
	if len(response.FileLists) != 1 {
		return remoteFileList{}, errors.New("QQ returned no file list")
	}
	return response.FileLists[0], nil
}

func (c *qfileClient) resolveDownloads(ctx context.Context, physicalIDs []string) map[string]directDownload {
	results := make(map[string]directDownload, len(physicalIDs))
	uniqueIDs := make([]string, 0, len(physicalIDs))
	seen := make(map[string]bool, len(physicalIDs))
	for _, physicalID := range physicalIDs {
		if physicalID != "" && !seen[physicalID] {
			seen[physicalID] = true
			uniqueIDs = append(uniqueIDs, physicalID)
		}
	}

	for start := 0; start < len(uniqueIDs); start += downloadBatch {
		end := start + downloadBatch
		if end > len(uniqueIDs) {
			end = len(uniqueIDs)
		}
		chunk := uniqueIDs[start:end]
		request := batchDownloadRequest{SceneType: 103}
		request.RequestHeader.Agent = 8
		for _, physicalID := range chunk {
			info := batchDownloadInfo{BatchID: physicalID, URLType: 2, DownloadScene: 0}
			info.Scene.BusinessType = 4
			info.Scene.AppType = 22
			info.Scene.SceneType = 5
			info.IndexNode.FileUUID = physicalID
			request.DownloadInfo = append(request.DownloadInfo, info)
		}

		var response batchDownloadResponse
		if err := c.postRPC(ctx, batchService, batchOIDB, request, &response); err != nil {
			for _, physicalID := range chunk {
				results[physicalID] = directDownload{Error: "requesting direct URL failed: " + err.Error()}
			}
			continue
		}

		responses := make(map[string]batchDownloadResult, len(response.DownloadResponses))
		for _, item := range response.DownloadResponses {
			responses[item.BatchID] = item
		}
		for _, physicalID := range chunk {
			response, ok := responses[physicalID]
			if !ok {
				results[physicalID] = directDownload{Error: "QQ omitted this file from its direct-download response"}
				continue
			}
			if strings.TrimSpace(response.RetCode) != "" && strings.TrimSpace(response.RetCode) != "0" {
				results[physicalID] = directDownload{Error: "QQ refused the download request: " + firstNonEmpty(response.RetMsg, response.RetCode)}
				continue
			}
			if response.URL == "" {
				results[physicalID] = directDownload{Error: "QQ returned an empty direct-download URL"}
				continue
			}
			results[physicalID] = directDownload{URL: response.URL}
		}
	}
	return results
}

func downloadAll(ctx context.Context, share *parsedShare, root string) ([]string, int) {
	downloaded := make([]string, 0, len(share.Files))
	failures := 0
	for index, file := range share.Files {
		label := printablePath(file.Path)
		if file.Error != "" {
			failures++
			fmt.Printf("[%d/%d] 跳过 %s：%s\n", index+1, len(share.Files), label, file.Error)
			continue
		}

		fmt.Printf("[%d/%d] 下载 %s\n", index+1, len(share.Files), label)
		destination, err := localDownloadPath(root, file.Path)
		if err != nil {
			failures++
			fmt.Printf("  失败：不能创建安全的本地文件名：%v\n", err)
			continue
		}
		directURLs := share.Client.resolveDownloads(ctx, []string{file.PhysicalID})
		direct, ok := directURLs[file.PhysicalID]
		if !ok {
			failures++
			fmt.Println("  失败：QQ 未返回下载地址。")
			continue
		}
		if direct.Error != "" {
			failures++
			fmt.Printf("  失败：%s\n", direct.Error)
			continue
		}

		savedPath, size, err := share.Client.downloadFile(ctx, withFilename(direct.URL, file.Name), destination)
		if err != nil {
			failures++
			fmt.Printf("  失败：%v\n", err)
			continue
		}
		downloaded = append(downloaded, savedPath)
		fmt.Printf("  已保存：%s（%d 字节）\n", savedPath, size)
	}
	return downloaded, failures
}

func (c *qfileClient) downloadFile(ctx context.Context, directURL, destination string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, directURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("create download request: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Referer", c.shareURL)
	resp, err := c.downloadClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("download request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, readErr := readLimited(resp.Body, maxPageBytes)
		if readErr != nil {
			return "", 0, fmt.Errorf("download returned %s", resp.Status)
		}
		return "", 0, fmt.Errorf("download returned %s: %s", resp.Status, compact(body, 400))
	}
	savedPath, size, err := writeNewFile(destination, resp.Body, defaultFileMode)
	if err != nil {
		return "", 0, fmt.Errorf("save download: %w", err)
	}
	return savedPath, size, nil
}

func localDownloadPath(root, remotePath string) (string, error) {
	components := strings.FieldsFunc(remotePath, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	if len(components) == 0 {
		return "", errors.New("QQ supplied an empty file path")
	}
	safeComponents := make([]string, 0, len(components))
	for _, component := range components {
		safeComponents = append(safeComponents, safeLocalComponent(component))
	}
	destination := filepath.Join(append([]string{root}, safeComponents...)...)
	if !pathWithin(root, destination) {
		return "", errors.New("file path would leave the current directory")
	}
	return destination, nil
}

func safeLocalComponent(value string) string {
	var builder strings.Builder
	for _, character := range value {
		switch {
		case character < 32:
			builder.WriteByte('_')
		case strings.ContainsRune(`<>:"/\\|?*`, character):
			builder.WriteByte('_')
		default:
			builder.WriteRune(character)
		}
	}
	name := strings.TrimRight(builder.String(), ". ")
	if name == "" || name == "." || name == ".." {
		return "_"
	}
	if isWindowsReservedName(name) {
		return "_" + name
	}
	return name
}

func isWindowsReservedName(name string) bool {
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	return false
}

func writeNewFile(destination string, source io.Reader, mode os.FileMode) (string, int64, error) {
	destination, err := chooseAvailablePath(destination)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), defaultFolderMode); err != nil {
		return "", 0, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".qfile-download-*.part")
	if err != nil {
		return "", 0, err
	}
	cleanUp := true
	defer func() {
		if cleanUp {
			_ = temporary.Close()
			_ = os.Remove(temporary.Name())
		}
	}()

	buffer := make([]byte, copyBufferSize)
	size, err := io.CopyBuffer(temporary, source, buffer)
	if err != nil {
		return "", 0, err
	}
	if err := temporary.Chmod(mode); err != nil {
		return "", 0, err
	}
	if err := temporary.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(temporary.Name(), destination); err != nil {
		return "", 0, err
	}
	cleanUp = false
	return destination, size, nil
}

func chooseAvailablePath(path string) (string, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return path, nil
	} else if err != nil {
		return "", err
	}

	directory := filepath.Dir(path)
	filename := filepath.Base(path)
	extension := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, extension)
	if stem == "" {
		stem, extension = filename, ""
	}
	for index := 1; index <= 10000; index++ {
		candidate := filepath.Join(directory, fmt.Sprintf("%s (%d)%s", stem, index, extension))
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not find an unused name for %q", path)
}

func processVolumeArchives(downloaded []string, root string) archiveReport {
	groups := findArchiveGroups(downloaded)
	report := archiveReport{found: len(groups)}
	for _, group := range groups {
		if len(group.parts) == 1 {
			fmt.Printf("解压压缩包：%s\n", group.basePath)
		} else {
			fmt.Printf("处理分卷压缩包：%s（%d 个分卷）\n", group.basePath, len(group.parts))
		}
		extractedFiles, err := extractVolumeGroup(group, root)
		report.files = append(report.files, extractedFiles...)
		report.extracted += len(extractedFiles)
		if err != nil {
			report.failures++
			fmt.Printf("  失败：%v\n", err)
			continue
		}
		report.completed++
		fmt.Printf("  完成：输出 %d 个文件。\n", len(extractedFiles))
	}
	return report
}

// findArchiveGroups returns every supported single-file archive as well as
// every supported numbered-volume group. A volume group claims its parts, so
// its base file is never decompressed a second time as a standalone archive.
func findArchiveGroups(downloaded []string) []volumeGroup {
	groups := findVolumeGroups(downloaded)
	claimed := make(map[string]bool, len(downloaded))
	for _, group := range groups {
		for _, part := range group.parts {
			claimed[filepath.Clean(part)] = true
		}
	}

	for _, downloadedPath := range downloaded {
		cleanPath := filepath.Clean(downloadedPath)
		if claimed[cleanPath] || archiveKindFor(cleanPath) == archiveUnknown {
			continue
		}
		groups = append(groups, volumeGroup{
			basePath: cleanPath,
			parts:    []string{cleanPath},
			kind:     archiveKindFor(cleanPath),
		})
		claimed[cleanPath] = true
	}
	sort.Slice(groups, func(left, right int) bool {
		return groups[left].basePath < groups[right].basePath
	})
	return groups
}

func findVolumeGroups(downloaded []string) []volumeGroup {
	present := make(map[string]bool, len(downloaded))
	partsByBase := make(map[string][]volumePart)
	for _, downloadedPath := range downloaded {
		cleanPath := filepath.Clean(downloadedPath)
		present[cleanPath] = true
		match := volumePartPattern.FindStringSubmatch(filepath.Base(cleanPath))
		if len(match) != 3 {
			continue
		}
		number, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			continue
		}
		basePath := filepath.Join(filepath.Dir(cleanPath), match[1])
		if archiveKindFor(basePath) == archiveUnknown {
			continue
		}
		partsByBase[basePath] = append(partsByBase[basePath], volumePart{path: cleanPath, number: number})
	}

	groups := make([]volumeGroup, 0, len(partsByBase))
	for basePath, numberedParts := range partsByBase {
		// A base file plus any numbered suffix is a valid split naming scheme
		// (for example a.tar and a.tar.002). If there is no base file, require
		// at least two numbered files to avoid treating an ordinary .001 file as
		// an archive volume.
		if present[basePath] {
			if len(numberedParts) == 0 {
				continue
			}
		} else if len(numberedParts) < 2 {
			continue
		}
		sort.Slice(numberedParts, func(left, right int) bool {
			return numberedParts[left].number < numberedParts[right].number
		})
		parts := make([]string, 0, len(numberedParts)+1)
		if present[basePath] {
			parts = append(parts, basePath)
		}
		for _, part := range numberedParts {
			parts = append(parts, part.path)
		}
		groups = append(groups, volumeGroup{
			basePath: basePath,
			parts:    parts,
			kind:     archiveKindFor(basePath),
		})
	}
	sort.Slice(groups, func(left, right int) bool {
		return groups[left].basePath < groups[right].basePath
	})
	return groups
}

func archiveKindFor(path string) archiveKind {
	name := strings.ToLower(filepath.Base(path))
	switch {
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		return archiveTarGzip
	case strings.HasSuffix(name, ".tar.xz"), strings.HasSuffix(name, ".txz"):
		return archiveTarXZ
	case strings.HasSuffix(name, ".tar.zstd"), strings.HasSuffix(name, ".tar.zst"), strings.HasSuffix(name, ".tzst"):
		return archiveTarZstd
	case strings.HasSuffix(name, ".tar"):
		return archiveTar
	case strings.HasSuffix(name, ".gz"):
		return archiveGzip
	case strings.HasSuffix(name, ".xz"):
		return archiveXZ
	case strings.HasSuffix(name, ".zstd"), strings.HasSuffix(name, ".zst"):
		return archiveZstd
	default:
		return archiveUnknown
	}
}

func extractVolumeGroup(group volumeGroup, root string) ([]string, error) {
	readers, closers, err := openVolumeReaders(group.parts)
	if err != nil {
		return nil, err
	}
	defer func() {
		for index := len(closers) - 1; index >= 0; index-- {
			_ = closers[index].Close()
		}
	}()
	merged := io.MultiReader(readers...)
	destinationDir := filepath.Dir(group.basePath)

	switch group.kind {
	case archiveTar:
		return extractTar(merged, destinationDir, root)
	case archiveTarGzip:
		reader, err := gzip.NewReader(merged)
		if err != nil {
			return nil, fmt.Errorf("open merged gzip stream: %w", err)
		}
		defer reader.Close()
		return extractTar(reader, destinationDir, root)
	case archiveTarXZ:
		reader, err := xz.NewReader(merged)
		if err != nil {
			return nil, fmt.Errorf("open merged xz stream: %w", err)
		}
		return extractTar(reader, destinationDir, root)
	case archiveTarZstd:
		reader, err := zstd.NewReader(merged)
		if err != nil {
			return nil, fmt.Errorf("open merged zstd stream: %w", err)
		}
		defer reader.Close()
		return extractTar(reader, destinationDir, root)
	case archiveGzip:
		reader, err := gzip.NewReader(merged)
		if err != nil {
			return nil, fmt.Errorf("open merged gzip stream: %w", err)
		}
		defer reader.Close()
		return decompressSingle(reader, decompressedPath(group.basePath, ".gz"))
	case archiveXZ:
		reader, err := xz.NewReader(merged)
		if err != nil {
			return nil, fmt.Errorf("open merged xz stream: %w", err)
		}
		return decompressSingle(reader, decompressedPath(group.basePath, ".xz"))
	case archiveZstd:
		reader, err := zstd.NewReader(merged)
		if err != nil {
			return nil, fmt.Errorf("open merged zstd stream: %w", err)
		}
		defer reader.Close()
		return decompressSingle(reader, decompressedPath(group.basePath, zstdSuffix(group.basePath)))
	default:
		return nil, errors.New("unsupported archive format")
	}
}

func zstdSuffix(path string) string {
	if strings.HasSuffix(strings.ToLower(filepath.Base(path)), ".zstd") {
		return ".zstd"
	}
	return ".zst"
}

func openVolumeReaders(paths []string) ([]io.Reader, []io.Closer, error) {
	readers := make([]io.Reader, 0, len(paths))
	closers := make([]io.Closer, 0, len(paths))
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			for index := len(closers) - 1; index >= 0; index-- {
				_ = closers[index].Close()
			}
			return nil, nil, fmt.Errorf("open volume %q: %w", path, err)
		}
		readers = append(readers, file)
		closers = append(closers, file)
	}
	return readers, closers, nil
}

func decompressSingle(reader io.Reader, destination string) ([]string, error) {
	path, _, err := writeNewFile(destination, reader, defaultFileMode)
	if err != nil {
		return nil, err
	}
	return []string{path}, nil
}

func decompressedPath(basePath, suffix string) string {
	filename := filepath.Base(basePath)
	if len(filename) >= len(suffix) && strings.EqualFold(filename[len(filename)-len(suffix):], suffix) {
		filename = filename[:len(filename)-len(suffix)]
	}
	if filename == "" {
		filename = "decompressed"
	}
	return filepath.Join(filepath.Dir(basePath), filename)
}

func extractTar(reader io.Reader, destinationDir, root string) ([]string, error) {
	tarReader := tar.NewReader(reader)
	extracted := make([]string, 0)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return extracted, nil
		}
		if err != nil {
			return extracted, fmt.Errorf("read tar stream: %w", err)
		}
		if header.Name == "" {
			continue
		}
		target, err := safeArchivePath(destinationDir, root, header.Name)
		if err != nil {
			return extracted, fmt.Errorf("unsafe archive entry %q: %w", header.Name, err)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, defaultFolderMode); err != nil {
				return extracted, fmt.Errorf("create directory %q: %w", target, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), defaultFolderMode); err != nil {
				return extracted, fmt.Errorf("create output directory: %w", err)
			}
			savedPath, _, err := writeNewFile(target, tarReader, defaultFileMode)
			if err != nil {
				return extracted, fmt.Errorf("extract %q: %w", header.Name, err)
			}
			extracted = append(extracted, savedPath)
		default:
			// Symlinks, hard links, device files, and other special entries are not
			// materialized. This prevents an archive from redirecting later writes.
			fmt.Printf("  跳过非普通归档项：%s\n", header.Name)
		}
	}
}

// displayFileHashes prints compatibility and integrity hashes for exactly the
// files created in this run: downloaded files plus successfully extracted files.
// Hashing is streamed, so even large files are never loaded fully into memory.
func displayFileHashes(root string, paths []string) {
	paths = uniqueFilePaths(paths)
	if len(paths) == 0 {
		fmt.Println("没有可计算哈希的文件。")
		return
	}
	fmt.Printf("正在计算 %d 个文件的 MD5、SHA-1、SHA-256：\n", len(paths))
	for _, path := range paths {
		displayPath := hashDisplayPath(root, path)
		values, err := calculateFileHashes(path)
		if err != nil {
			fmt.Printf("- %s\n  哈希失败：%v\n", displayPath, err)
			continue
		}
		fmt.Printf("- %s（%d 字节）\n  MD5: %s\n  SHA-1: %s\n  SHA-256: %s\n", displayPath, values.size, values.md5, values.sha1, values.sha256)
	}
}

func uniqueFilePaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	unique := make([]string, 0, len(paths))
	for _, path := range paths {
		cleanPath := filepath.Clean(path)
		if cleanPath == "." || seen[cleanPath] {
			continue
		}
		seen[cleanPath] = true
		unique = append(unique, cleanPath)
	}
	return unique
}

func hashDisplayPath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative) {
		return relative
	}
	return path
}

func calculateFileHashes(path string) (fileHashValues, error) {
	file, err := os.Open(path)
	if err != nil {
		return fileHashValues{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fileHashValues{}, err
	}
	if !info.Mode().IsRegular() {
		return fileHashValues{}, errors.New("not a regular file")
	}

	md5Hash := md5.New()   // #nosec G401 -- user requested an MD5 checksum, not authentication.
	sha1Hash := sha1.New() // #nosec G401 -- user requested a SHA-1 checksum, not authentication.
	sha256Hash := sha256.New()
	buffer := make([]byte, copyBufferSize)
	size, err := io.CopyBuffer(io.MultiWriter(md5Hash, sha1Hash, sha256Hash), file, buffer)
	if err != nil {
		return fileHashValues{}, err
	}
	return fileHashValues{
		size:   size,
		md5:    hex.EncodeToString(md5Hash.Sum(nil)),
		sha1:   hex.EncodeToString(sha1Hash.Sum(nil)),
		sha256: hex.EncodeToString(sha256Hash.Sum(nil)),
	}, nil
}

func safeArchivePath(destinationDir, root, archiveName string) (string, error) {
	if strings.ContainsRune(archiveName, 0) {
		return "", errors.New("contains NUL")
	}
	normalized := strings.ReplaceAll(archiveName, "\\", "/")
	clean := filepath.Clean(filepath.FromSlash(normalized))
	if clean == "." || filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" {
		return "", errors.New("path is not relative")
	}
	target := filepath.Join(destinationDir, clean)
	if !pathWithin(destinationDir, target) || !pathWithin(root, target) {
		return "", errors.New("path escapes extraction directory")
	}
	return target, nil
}

func pathWithin(base, target string) bool {
	absoluteBase, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	absoluteTarget, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(absoluteBase, absoluteTarget)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}

func (c *qfileClient) postRPC(ctx context.Context, service, oidb string, payload, destination any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	nonce, err := newNonce()
	if err != nil {
		return err
	}
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha1.New, []byte(protocolHMACKey))
	_, _ = mac.Write(body)
	_, _ = mac.Write([]byte(nonce))
	_, _ = mac.Write([]byte(timestamp))
	signature := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcBase+service, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("create %s request: %w", service, err)
	}
	command, serviceType, ok := strings.Cut(oidb, "_")
	if !ok {
		return fmt.Errorf("invalid OIDB mapping for %s", service)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Referer", c.shareURL)
	req.Header.Set("X-Device-Id", c.deviceID)
	req.Header.Set("X-OIDB", fmt.Sprintf(`{"uint32_command":"%s", "uint32_service_type":"%s"}`, command, serviceType))
	req.Header.Set("X-QQ-AR-Nonce", nonce)
	req.Header.Set("X-QQ-AR-Timestamp", timestamp)
	req.Header.Set("X-QQ-AR-Signature", signature)
	req.Header.Set("Cookie", "uin=9000002; p_uin=9000002; deviceId="+c.deviceID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", service, err)
	}
	defer resp.Body.Close()
	responseBody, err := readLimited(resp.Body, maxRPCBytes)
	if err != nil {
		return fmt.Errorf("read %s response: %w", service, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%s returned %s: %s", service, resp.Status, compact(responseBody, 400))
	}

	var envelope rpcEnvelope
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return fmt.Errorf("decode %s response: %w", service, err)
	}
	if !isZeroJSON(envelope.RetCode) || !isZeroJSON(envelope.Error.Code) {
		message := firstNonEmpty(envelope.Error.Message, envelope.Message, "unknown QQ API error")
		return fmt.Errorf("%s failed (%s/%s): %s", service, printableCode(envelope.RetCode), printableCode(envelope.Error.Code), message)
	}
	if destination == nil {
		return nil
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("%s returned no data", service)
	}
	if err := json.Unmarshal(envelope.Data, destination); err != nil {
		return fmt.Errorf("decode %s data: %w", service, err)
	}
	return nil
}

func newNonce() (string, error) {
	max := big.NewInt(10000)
	number, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", fmt.Errorf("generate request nonce: %w", err)
	}
	return fmt.Sprintf("%d", number.Int64()+1), nil
}

func findCookie(cookies []*http.Cookie, name string) string {
	for _, cookie := range cookies {
		if strings.EqualFold(cookie.Name, name) {
			return cookie.Value
		}
	}
	return ""
}

func readLimited(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("response exceeds %d-byte safety limit", maximum)
	}
	return data, nil
}

func isZeroJSON(value json.RawMessage) bool {
	switch strings.TrimSpace(string(value)) {
	case "", "0", `"0"`, "null":
		return true
	default:
		return false
	}
}

func printableCode(value json.RawMessage) string {
	value = json.RawMessage(strings.TrimSpace(string(value)))
	if len(value) == 0 {
		return "0"
	}
	return string(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func withFilename(directURL, name string) string {
	if directURL == "" || name == "" {
		return directURL
	}
	separator := "&"
	if !strings.Contains(directURL, "?") {
		separator = "?"
	}
	// encodeURIComponent, used by QQ's page, represents spaces as %20 rather
	// than '+'. Both work on most servers, but matching it is safer here.
	filename := strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
	return directURL + separator + "filename=" + filename
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	if name == "" {
		return parent
	}
	return parent + "/" + name
}

func printablePath(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

func compact(data []byte, limit int) string {
	text := strings.Join(strings.Fields(string(data)), " ")
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

func writeText(writer io.Writer, result output) {
	for _, file := range result.Files {
		if file.Error != "" {
			fmt.Fprintf(writer, "%s\tERROR: %s\n", file.Path, file.Error)
			continue
		}
		fmt.Fprintf(writer, "%s\t%s\n", file.Path, file.DownloadURL)
	}
}
