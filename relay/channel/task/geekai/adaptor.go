package geekai

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
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
	apiKey      string
	baseURL     string
}

type ComicStudioRequest struct {
	Model          string   `json:"model"`
	Prompt         string   `json:"prompt"`
	Duration       int      `json:"duration"`
	AspectRatio    string   `json:"aspect_ratio"`
	Size           string   `json:"size"`
	ImageURLs      []string `json:"image_urls"`
	VideoURLs      []string `json:"video_urls"`
	AudioURLs      []string `json:"audio_urls"`
	GenerateAudio  bool     `json:"generate_audio"`
	EnableWebSearch bool    `json:"enable_web_search,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

type GeekAIResponse struct {
	ID                  string                 `json:"id"`
	Object              string                 `json:"object"`
	Model               string                 `json:"model"`
	Status              string                 `json:"status"`
	Progress            int                    `json:"progress"`
	CreatedAt           int64                  `json:"created_at"`
	CompletedAt         int64                  `json:"completed_at,omitempty"`
	ExpiresAt           int64                  `json:"expires_at,omitempty"`
	Seconds             string                 `json:"seconds"`
	Size                string                 `json:"size"`
	VideoURL            string                 `json:"video_url,omitempty"`
	RemixedFromVideoID  string                 `json:"remixed_from_video_id,omitempty"`
	Error               *GeekAIError          `json:"error,omitempty"`
}

type GeekAIError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.ChannelType = info.ChannelType
	a.baseURL = info.ChannelBaseUrl
	a.apiKey = info.ApiKey
}

func cleanImageURL(url string) string {
	url = strings.TrimSpace(url)
	url = strings.Trim(url, "`")
	url = strings.Trim(url, "\"")
	url = strings.Trim(url, "'")
	url = strings.TrimSpace(url)
	return url
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) (taskErr *dto.TaskError) {
	var req ComicStudioRequest
	bodyStorage, _ := common.GetBodyStorage(c)
	if err := common.Unmarshal(bodyStorage, &req); err != nil {
		return service.TaskErrorWrapper(err, "parse_request_failed", http.StatusBadRequest)
	}

	logger.LogInfo(c, fmt.Sprintf("GeekAI create request - model: %s, size: %s, aspect_ratio: %s, duration: %d", req.Model, req.Size, req.AspectRatio, req.Duration))

	if len(req.VideoURLs) > 0 {
		return service.TaskErrorWrapperLocal(fmt.Errorf("当前上游接口不支持 video_urls"), "invalid_request", http.StatusBadRequest)
	}

	if len(req.AudioURLs) > 0 {
		return service.TaskErrorWrapperLocal(fmt.Errorf("当前上游接口不支持 audio_urls"), "invalid_request", http.StatusBadRequest)
	}

	if len(req.ImageURLs) == 0 {
		return service.TaskErrorWrapperLocal(fmt.Errorf("image_urls 不能为空"), "invalid_request", http.StatusBadRequest)
	}

	if req.GenerateAudio {
		logger.LogWarn(c, "收到 generate_audio=true 但上游不支持该字段，已忽略")
	}

	if req.EnableWebSearch {
		logger.LogWarn(c, "收到 enable_web_search=true 但上游不支持该字段，已忽略")
	}

	info.Action = constant.TaskActionGenerate
	return nil
}

func (a *TaskAdaptor) BuildRequestURL(info *relaycommon.RelayInfo) (string, error) {
	return fmt.Sprintf("%s/v1/videos", a.baseURL), nil
}

func (a *TaskAdaptor) BuildRequestHeader(c *gin.Context, req *http.Request, info *relaycommon.RelayInfo) error {
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	return nil
}

func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	var req ComicStudioRequest
	bodyStorage, _ := common.GetBodyStorage(c)
	if err := common.Unmarshal(bodyStorage, &req); err != nil {
		return nil, err
	}

	rawImgURL := req.ImageURLs[0]
	cleanImgURL := cleanImageURL(rawImgURL)
	logger.LogInfo(c, fmt.Sprintf("原始 image_urls[0]: %q", rawImgURL))
	logger.LogInfo(c, fmt.Sprintf("清洗后 image_urls[0]: %q", cleanImgURL))

	imgData, err := a.downloadImage(c, cleanImgURL)
	if err != nil {
		return nil, err
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if err := writer.WriteField("model", req.Model); err != nil {
		return nil, err
	}
	if err := writer.WriteField("prompt", req.Prompt); err != nil {
		return nil, err
	}
	if err := writer.WriteField("aspect_ratio", req.AspectRatio); err != nil {
		return nil, err
	}
	if err := writer.WriteField("size", req.Size); err != nil {
		return nil, err
	}
	if err := writer.WriteField("seconds", fmt.Sprintf("%d", req.Duration)); err != nil {
		return nil, err
	}

	part, err := writer.CreateFormFile("input_reference", "reference.png")
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(imgData); err != nil {
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	c.Set("content-type", writer.FormDataContentType())

	logger.LogInfo(c, fmt.Sprintf("上游请求字段: model=%s, aspect_ratio=%s, size=%s, seconds=%d, 包含 input_reference 文件", req.Model, req.AspectRatio, req.Size, req.Duration))

	return body, nil
}

func (a *TaskAdaptor) downloadImage(c *gin.Context, url string) ([]byte, error) {
	logger.LogInfo(c, fmt.Sprintf("开始下载图片: %s", url))
	
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(fmt.Errorf("下载 image_urls[0] 失败，无法转换为上游 input_reference 文件: %v", err), "download_failed", http.StatusInternalServerError)
	}

	client, err := service.GetHttpClient()
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(fmt.Errorf("下载 image_urls[0] 失败，无法转换为上游 input_reference 文件: %v", err), "download_failed", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, service.TaskErrorWrapperLocal(fmt.Errorf("下载 image_urls[0] 失败，HTTP 状态码: %d", resp.StatusCode), "download_failed", http.StatusInternalServerError)
	}

	imgData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(fmt.Errorf("下载 image_urls[0] 失败，读取响应失败: %v", err), "download_failed", http.StatusInternalServerError)
	}

	logger.LogInfo(c, fmt.Sprintf("图片下载成功 - 状态码: %d, Content-Type: %s, 大小: %d 字节", resp.StatusCode, resp.Header.Get("Content-Type"), len(imgData)))

	return imgData, nil
}

func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	url, err := a.BuildRequestURL(info)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", url, requestBody)
	if err != nil {
		return nil, err
	}

	if err := a.BuildRequestHeader(c, req, info); err != nil {
		return nil, err
	}

	if contentType, exists := c.Get("content-type"); exists {
		req.Header.Set("Content-Type", contentType.(string))
	}

	client, err := service.GetHttpClient()
	if err != nil {
		return nil, err
	}

	return client.Do(req)
}

func (a *TaskAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (taskID string, taskData []byte, taskErr *dto.TaskError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		taskErr = service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
		return
	}

	logger.LogDebug(c, fmt.Sprintf("GeekAI upstream response: %s", string(responseBody)))

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		taskErr = service.TaskErrorWrapperLocal(fmt.Errorf("上游返回错误: %s", string(responseBody)), "upstream_error", resp.StatusCode)
		return
	}

	var geekResp GeekAIResponse
	if err := common.Unmarshal(responseBody, &geekResp); err != nil {
		taskErr = service.TaskErrorWrapper(err, "unmarshal_response_failed", http.StatusInternalServerError)
		return
	}

	ov := dto.NewOpenAIVideo()
	ov.ID = info.PublicTaskID
	ov.TaskID = info.PublicTaskID
	ov.Object = "video"
	ov.Model = info.OriginModelName
	ov.Status = dto.VideoStatusQueued
	ov.Progress = 0
	ov.CreatedAt = time.Now().Unix()
	ov.Seconds = geekResp.Seconds
	ov.Size = geekResp.Size

	c.JSON(http.StatusOK, ov)

	upstreamTaskID := geekResp.ID
	if upstreamTaskID == "" {
		upstreamTaskID = info.PublicTaskID
	}

	return upstreamTaskID, responseBody, nil
}

func (a *TaskAdaptor) FetchTask(baseUrl, key string, body map[string]any, proxy string) (*http.Response, error) {
	taskID, ok := body["task_id"].(string)
	if !ok {
		return nil, fmt.Errorf("invalid task_id")
	}

	url := fmt.Sprintf("%s/v1/videos/%s", baseUrl, taskID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+key)

	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	return client.Do(req)
}

func (a *TaskAdaptor) GetModelList() []string {
	return []string{"veo_3_1-fast", "grok-video-3", "grok-video-3-pro"}
}

func (a *TaskAdaptor) GetChannelName() string {
	return "geekai"
}

func (a *TaskAdaptor) ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error) {
	taskInfo := &relaycommon.TaskInfo{}
	
	var geekResp GeekAIResponse
	if err := common.Unmarshal(respBody, &geekResp); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal response body")
	}

	taskInfo.TaskID = geekResp.ID
	taskInfo.Reason = ""

	switch geekResp.Status {
	case "queued":
		taskInfo.Status = model.TaskStatusQueued
	case "processing":
		taskInfo.Status = model.TaskStatusInProgress
	case "completed":
		taskInfo.Status = model.TaskStatusSuccess
		taskInfo.Url = geekResp.VideoURL
	case "failed", "cancelled":
		taskInfo.Status = model.TaskStatusFailure
		if geekResp.Error != nil {
			taskInfo.Reason = geekResp.Error.Message
		}
	default:
		taskInfo.Status = model.TaskStatusQueued
	}

	return taskInfo, nil
}

func (a *TaskAdaptor) ConvertToOpenAIVideo(originTask *model.Task) ([]byte, error) {
	openAIVideo := dto.NewOpenAIVideo()
	openAIVideo.ID = originTask.TaskID
	openAIVideo.Status = originTask.Status.ToVideoStatus()
	openAIVideo.SetProgressStr(originTask.Progress)
	openAIVideo.CreatedAt = originTask.CreatedAt
	openAIVideo.Model = originTask.Properties.OriginModelName

	var geekResp GeekAIResponse
	if err := common.Unmarshal(originTask.Data, &geekResp); err == nil {
		openAIVideo.Seconds = geekResp.Seconds
		openAIVideo.Size = geekResp.Size
		openAIVideo.CompletedAt = geekResp.CompletedAt
		openAIVideo.ExpiresAt = geekResp.ExpiresAt
		openAIVideo.RemixedFromVideoID = geekResp.RemixedFromVideoID

		if geekResp.VideoURL != "" {
			openAIVideo.SetMetadata("url", geekResp.VideoURL)
			openAIVideo.SetMetadata("videos", []map[string]string{{"url": geekResp.VideoURL}})
		}

		if geekResp.Error != nil {
			openAIVideo.Error = &dto.OpenAIVideoError{
				Message: geekResp.Error.Message,
				Code:    geekResp.Error.Code,
			}
		}
	}

	if originTask.PrivateData.ResultURL != "" {
		openAIVideo.SetMetadata("url", originTask.PrivateData.ResultURL)
		openAIVideo.SetMetadata("videos", []map[string]string{{"url": originTask.PrivateData.ResultURL}})
	}

	return common.Marshal(openAIVideo)
}
