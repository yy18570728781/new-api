package geekai

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	taskcommon "github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

type TaskAdaptor struct {
	taskcommon.BaseBilling
	ChannelType int
	baseURL     string
	apiKey      string
}

type comicStudioRequest struct {
	Model           string         `json:"model"`
	Prompt          string         `json:"prompt"`
	Duration        int            `json:"duration"`
	AspectRatio     string         `json:"aspect_ratio"`
	Size            string         `json:"size"`
	ImageURLs       []string       `json:"image_urls"`
	VideoURLs       []string       `json:"video_urls"`
	AudioURLs       []string       `json:"audio_urls"`
	GenerateAudio   bool           `json:"generate_audio"`
	EnableWebSearch bool           `json:"enable_web_search,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

type geekAIError struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

type geekAIVideo struct {
	URL string `json:"url"`
}

type geekAIResponse struct {
	ID                 string        `json:"id"`
	Object             string        `json:"object"`
	Model              string        `json:"model"`
	Status             string        `json:"status"`
	Progress           int           `json:"progress"`
	CreatedAt          int64         `json:"created_at"`
	CompletedAt        int64         `json:"completed_at,omitempty"`
	ExpiresAt          int64         `json:"expires_at,omitempty"`
	Seconds            string        `json:"seconds,omitempty"`
	Size               string        `json:"size,omitempty"`
	VideoURL           string        `json:"video_url,omitempty"`
	Videos             []geekAIVideo `json:"videos,omitempty"`
	RemixedFromVideoID string        `json:"remixed_from_video_id,omitempty"`
	Error              *geekAIError  `json:"error,omitempty"`
}

type preparedRequest struct {
	Request    comicStudioRequest
	CleanImage string
	MappedSize string
	Seconds    int
	UpSample   bool
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.ChannelType = info.ChannelType
	a.baseURL = strings.TrimRight(info.ChannelBaseUrl, "/")
	a.apiKey = info.ApiKey
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *dto.TaskError {
	_, taskErr := a.prepareRequest(c, info, false)
	if taskErr != nil {
		return taskErr
	}
	info.Action = constant.TaskActionGenerate
	return nil
}

func (a *TaskAdaptor) BuildRequestURL(info *relaycommon.RelayInfo) (string, error) {
	return a.baseURL + "/v1/videos", nil
}

func (a *TaskAdaptor) BuildRequestHeader(c *gin.Context, req *http.Request, info *relaycommon.RelayInfo) error {
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Accept", "application/json")
	if contentType, ok := c.Get("task_request_content_type"); ok {
		req.Header.Set("Content-Type", contentType.(string))
	}
	return nil
}

func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	prepared, taskErr := a.prepareRequest(c, info, true)
	if taskErr != nil {
		return nil, taskErr.Error
	}

	imageData, fileName, taskErr := a.downloadReferenceImage(c, prepared.CleanImage)
	if taskErr != nil {
		return nil, taskErr.Error
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("model", prepared.Request.Model); err != nil {
		return nil, err
	}
	if err := writer.WriteField("prompt", prepared.Request.Prompt); err != nil {
		return nil, err
	}
	if err := writer.WriteField("size", prepared.MappedSize); err != nil {
		return nil, err
	}
	if err := writer.WriteField("seconds", strconv.Itoa(prepared.Seconds)); err != nil {
		return nil, err
	}
	if err := writer.WriteField("enable_upsample", strconv.FormatBool(prepared.UpSample)); err != nil {
		return nil, err
	}

	part, err := writer.CreateFormFile("input_reference", fileName)
	if err != nil {
		return nil, err
	}
	if _, err = part.Write(imageData); err != nil {
		return nil, err
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}

	c.Set("task_request_content_type", writer.FormDataContentType())
	logger.LogInfo(c, fmt.Sprintf("GeekAI upstream request: path=/v1/videos content_type=%s fields=[model prompt size seconds enable_upsample input_reference]", writer.FormDataContentType()))
	return bytes.NewReader(body.Bytes()), nil
}

func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, requestBody)
}

func (a *TaskAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (taskID string, taskData []byte, taskErr *dto.TaskError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}

	if isHTMLResponse(responseBody) {
		return "", nil, service.TaskErrorWrapperLocal(
			fmt.Errorf("上游返回 HTML 页面，疑似请求地址错误或服务未正确配置"),
			"upstream_error",
			http.StatusBadGateway,
		)
	}

	var upstream geekAIResponse
	if err = common.Unmarshal(responseBody, &upstream); err != nil {
		return "", nil, service.TaskErrorWrapper(errors.Wrap(err, string(responseBody)), "unmarshal_response_failed", http.StatusInternalServerError)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := "上游生成失败"
		if upstream.Error != nil && strings.TrimSpace(upstream.Error.Message) != "" {
			message = upstream.Error.Message
		} else if strings.TrimSpace(string(responseBody)) != "" {
			message = string(responseBody)
		}
		return "", nil, service.TaskErrorWrapperLocal(fmt.Errorf("%s", message), "upstream_error", resp.StatusCode)
	}

	openAIVideo := dto.NewOpenAIVideo()
	openAIVideo.ID = info.PublicTaskID
	openAIVideo.TaskID = info.PublicTaskID
	openAIVideo.Model = info.OriginModelName
	openAIVideo.Status = mapVideoStatus(upstream.Status)
	openAIVideo.Progress = upstream.Progress
	openAIVideo.CreatedAt = chooseTime(upstream.CreatedAt, time.Now().Unix())
	openAIVideo.Seconds = upstream.Seconds
	openAIVideo.Size = upstream.Size

	c.JSON(http.StatusOK, openAIVideo)

	if strings.TrimSpace(upstream.ID) == "" {
		return info.PublicTaskID, responseBody, nil
	}
	return upstream.ID, responseBody, nil
}

func (a *TaskAdaptor) FetchTask(baseURL, key string, body map[string]any, proxy string) (*http.Response, error) {
	taskID, ok := body["task_id"].(string)
	if !ok || strings.TrimSpace(taskID) == "" {
		return nil, fmt.Errorf("invalid task_id")
	}

	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/v1/videos/"+taskID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	return client.Do(req)
}

func (a *TaskAdaptor) ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error) {
	if isHTMLResponse(respBody) {
		return relaycommon.FailTaskInfo("上游返回 HTML 页面，疑似查询路径错误或服务未正确配置"), nil
	}

	var upstream geekAIResponse
	if err := common.Unmarshal(respBody, &upstream); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal response body")
	}

	taskInfo := &relaycommon.TaskInfo{
		TaskID:   upstream.ID,
		Status:   mapTaskStatus(upstream.Status),
		Progress: formatProgress(upstream.Progress, upstream.Status),
	}

	videoURL := firstVideoURL(upstream)
	if videoURL != "" {
		taskInfo.Url = videoURL
		taskInfo.RemoteUrl = videoURL
	}
	if upstream.Error != nil {
		taskInfo.Reason = upstream.Error.Message
	}
	return taskInfo, nil
}

func (a *TaskAdaptor) ConvertToOpenAIVideo(originTask *model.Task) ([]byte, error) {
	resp := map[string]any{
		"id":         originTask.TaskID,
		"object":     "video",
		"model":      originTask.Properties.OriginModelName,
		"status":     originTask.Status.ToVideoStatus(),
		"progress":   progressToInt(originTask.Progress),
		"created_at": originTask.CreatedAt,
	}

	var upstream geekAIResponse
	if err := common.Unmarshal(originTask.Data, &upstream); err == nil {
		if upstream.Seconds != "" {
			resp["seconds"] = upstream.Seconds
		}
		if upstream.Size != "" {
			resp["size"] = upstream.Size
		}
		if upstream.CompletedAt > 0 {
			resp["completed_at"] = upstream.CompletedAt
		}
		if upstream.ExpiresAt > 0 {
			resp["expires_at"] = upstream.ExpiresAt
		}
		if upstream.RemixedFromVideoID != "" {
			resp["remixed_from_video_id"] = upstream.RemixedFromVideoID
		}
		if upstream.Error != nil && upstream.Error.Message != "" {
			resp["error"] = upstream.Error
		}
	}

	videoURL := originTask.PrivateData.ResultURL
	if videoURL == "" {
		videoURL = firstVideoURL(upstream)
	}
	if videoURL != "" {
		resp["videos"] = []map[string]string{{"url": videoURL}}
	}

	return common.Marshal(resp)
}

func (a *TaskAdaptor) GetModelList() []string {
	return []string{"veo_3_1-fast", "veo_3_1-fast-fl", "grok-video-3", "grok-video-3-pro"}
}

func (a *TaskAdaptor) GetChannelName() string {
	return "geekai"
}

func (a *TaskAdaptor) prepareRequest(c *gin.Context, info *relaycommon.RelayInfo, verbose bool) (*preparedRequest, *dto.TaskError) {
	var req comicStudioRequest
	if err := common.UnmarshalBodyReusable(c, &req); err != nil {
		return nil, service.TaskErrorWrapper(err, "invalid_request", http.StatusBadRequest)
	}

	if verbose {
		bodyStorage, err := common.GetBodyStorage(c)
		if err == nil {
			if rawBody, readErr := bodyStorage.Bytes(); readErr == nil {
				logger.LogInfo(c, fmt.Sprintf("GeekAI create request path=/v1/videos/generations model=%s body=%s", req.Model, string(rawBody)))
			}
		}
	}

	if len(req.ImageURLs) == 0 {
		return nil, service.TaskErrorWrapperLocal(fmt.Errorf("image_urls 缺失，至少需要提供一张参考图"), "invalid_request", http.StatusBadRequest)
	}
	if len(req.VideoURLs) > 0 {
		return nil, service.TaskErrorWrapperLocal(fmt.Errorf("veo_3_1-fast 当前上游接口不支持 video_urls"), "invalid_request", http.StatusBadRequest)
	}
	if len(req.AudioURLs) > 0 {
		return nil, service.TaskErrorWrapperLocal(fmt.Errorf("veo_3_1-fast 当前上游接口不支持 audio_urls"), "invalid_request", http.StatusBadRequest)
	}

	seconds := req.Duration
	if seconds == 0 {
		seconds = 8
	}
	if seconds != 8 {
		return nil, service.TaskErrorWrapperLocal(fmt.Errorf("%s 当前仅支持 8 秒视频，请求 duration=%d 不被上游支持", taskcommon.DefaultString(req.Model, "veo_3_1-fast"), req.Duration), "invalid_request", http.StatusBadRequest)
	}

	mappedSize, err := mapSize(req.Size, req.AspectRatio)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}

	cleanImage := cleanReferenceURL(req.ImageURLs[0])
	if cleanImage == "" {
		return nil, service.TaskErrorWrapperLocal(fmt.Errorf("image_urls[0] 为空，无法转换为上游 input_reference 文件"), "invalid_request", http.StatusBadRequest)
	}

	logger.LogInfo(c, fmt.Sprintf("GeekAI request summary: model=%s raw_image=%q cleaned_image=%q raw_size=%s raw_aspect_ratio=%s mapped_size=%s raw_duration=%d seconds=%d enable_upsample=%v",
		req.Model, req.ImageURLs[0], cleanImage, req.Size, req.AspectRatio, mappedSize, req.Duration, seconds, false))
	if req.GenerateAudio {
		logger.LogWarn(c, "收到 generate_audio，但当前上游未透传该字段")
	}
	if req.EnableWebSearch {
		logger.LogWarn(c, "收到 enable_web_search，但当前上游未透传该字段")
	}

	info.Action = constant.TaskActionGenerate
	return &preparedRequest{
		Request:    req,
		CleanImage: cleanImage,
		MappedSize: mappedSize,
		Seconds:    seconds,
		UpSample:   false,
	}, nil
}

func (a *TaskAdaptor) downloadReferenceImage(c *gin.Context, rawURL string) ([]byte, string, *dto.TaskError) {
	logger.LogInfo(c, fmt.Sprintf("GeekAI download reference image: url=%s", rawURL))

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", service.TaskErrorWrapperLocal(fmt.Errorf("下载 image_urls[0] 失败，无法转换为上游 input_reference 文件"), "download_failed", http.StatusBadRequest)
	}

	resp, err := service.GetHttpClient().Do(req)
	if err != nil {
		return nil, "", service.TaskErrorWrapperLocal(fmt.Errorf("下载 image_urls[0] 失败，无法转换为上游 input_reference 文件"), "download_failed", http.StatusBadGateway)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", service.TaskErrorWrapperLocal(fmt.Errorf("下载 image_urls[0] 失败，无法转换为上游 input_reference 文件"), "download_failed", http.StatusBadGateway)
	}

	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if !strings.HasPrefix(contentType, "image/") {
		return nil, "", service.TaskErrorWrapperLocal(fmt.Errorf("下载 image_urls[0] 失败，目标资源不是图片"), "download_failed", http.StatusBadRequest)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", service.TaskErrorWrapperLocal(fmt.Errorf("下载 image_urls[0] 失败，无法转换为上游 input_reference 文件"), "download_failed", http.StatusBadGateway)
	}

	fileName := buildFileName(rawURL, contentType)
	logger.LogInfo(c, fmt.Sprintf("GeekAI image downloaded: status=%d mime=%s size=%d filename=%s", resp.StatusCode, contentType, len(data), fileName))
	return data, fileName, nil
}

func cleanReferenceURL(raw string) string {
	s := strings.TrimSpace(raw)
	for {
		next := strings.TrimSpace(strings.Trim(strings.Trim(strings.Trim(s, "`"), `"`), `'`))
		if next == s {
			return next
		}
		s = next
	}
}

func mapSize(size, aspectRatio string) (string, error) {
	size = strings.TrimSpace(strings.ToLower(size))
	aspectRatio = strings.TrimSpace(aspectRatio)

	if strings.Contains(size, "x") {
		return size, nil
	}
	switch size {
	case "720p":
		switch aspectRatio {
		case "16:9":
			return "1280x720", nil
		case "9:16":
			return "720x1280", nil
		}
	}
	return "", fmt.Errorf("无法将 size=%s、aspect_ratio=%s 映射为上游 VEO 所需分辨率", size, aspectRatio)
}

func buildFileName(rawURL, contentType string) string {
	ext := extensionFromContentType(contentType)
	if parsed, err := url.Parse(rawURL); err == nil {
		base := path.Base(parsed.Path)
		if base != "" && base != "." && base != "/" {
			if path.Ext(base) != "" {
				return base
			}
			if ext != "" {
				return base + ext
			}
			return base
		}
	}
	if ext == "" {
		ext = ".png"
	}
	return "reference" + ext
}

func extensionFromContentType(contentType string) string {
	switch {
	case strings.Contains(contentType, "png"):
		return ".png"
	case strings.Contains(contentType, "jpeg"), strings.Contains(contentType, "jpg"):
		return ".jpg"
	case strings.Contains(contentType, "webp"):
		return ".webp"
	case strings.Contains(contentType, "gif"):
		return ".gif"
	default:
		return ""
	}
}

func isHTMLResponse(body []byte) bool {
	text := strings.TrimSpace(strings.ToLower(string(body)))
	return strings.HasPrefix(text, "<!doctype html") || strings.HasPrefix(text, "<html")
}

func mapTaskStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "queued", "submitted":
		return string(model.TaskStatusQueued)
	case "processing", "running", "in_progress":
		return string(model.TaskStatusInProgress)
	case "completed", "success", "succeeded":
		return string(model.TaskStatusSuccess)
	case "failed", "cancelled", "canceled", "error":
		return string(model.TaskStatusFailure)
	default:
		return string(model.TaskStatusSubmitted)
	}
}

func mapVideoStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "success", "succeeded":
		return dto.VideoStatusCompleted
	case "failed", "cancelled", "canceled", "error":
		return dto.VideoStatusFailed
	case "processing", "running", "in_progress":
		return dto.VideoStatusInProgress
	default:
		return dto.VideoStatusQueued
	}
}

func formatProgress(progress int, status string) string {
	if progress <= 0 {
		switch mapVideoStatus(status) {
		case dto.VideoStatusCompleted:
			return "100%"
		case dto.VideoStatusInProgress:
			return "50%"
		default:
			return "0%"
		}
	}
	if progress > 100 {
		progress = 100
	}
	return fmt.Sprintf("%d%%", progress)
}

func progressToInt(progress string) int {
	progress = strings.TrimSpace(strings.TrimSuffix(progress, "%"))
	v, _ := strconv.Atoi(progress)
	return v
}

func firstVideoURL(resp geekAIResponse) string {
	if resp.VideoURL != "" {
		return resp.VideoURL
	}
	if len(resp.Videos) > 0 {
		return resp.Videos[0].URL
	}
	return ""
}

func chooseTime(v, fallback int64) int64 {
	if v > 0 {
		return v
	}
	return fallback
}
