package grsai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

type imageRequest struct {
	Model        string   `json:"model"`
	Prompt       string   `json:"prompt"`
	AspectRatio  string   `json:"aspectRatio,omitempty"`
	Quality      string   `json:"quality,omitempty"`
	ImageSize    string   `json:"imageSize,omitempty"`
	Urls         []string `json:"urls,omitempty"`
	WebHook      string   `json:"webHook,omitempty"`
	ShutProgress bool     `json:"shutProgress,omitempty"`
}

type submitResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
}

type taskResultItem struct {
	URL     string `json:"url"`
	Content string `json:"content"`
}

type taskResult struct {
	ID            string           `json:"id"`
	URL           string           `json:"url"`
	Results       []taskResultItem `json:"results"`
	Progress      int              `json:"progress"`
	Status        string           `json:"status"`
	FailureReason string           `json:"failure_reason"`
	Error         string           `json:"error"`
}

type resultResponse struct {
	Code int        `json:"code"`
	Msg  string     `json:"msg"`
	Data taskResult `json:"data"`
}

func buildImageRequest(info *relaycommon.RelayInfo, request dto.ImageRequest) (*imageRequest, error) {
	modelName := strings.TrimSpace(info.UpstreamModelName)
	if modelName == "" {
		modelName = strings.TrimSpace(request.Model)
	}
	if modelName == "" {
		return nil, fmt.Errorf("model is required")
	}

	grsReq := &imageRequest{
		Model:        modelName,
		Prompt:       request.Prompt,
		Urls:         extractImageURLs(request),
		WebHook:      "-1",
		ShutProgress: true,
	}

	if isNanoBananaModel(modelName) {
		grsReq.AspectRatio = nanoBananaAspectRatio(request)
		if imageSize := extractString(request.Extra, "imageSize", "image_size"); imageSize != "" {
			grsReq.ImageSize = imageSize
		} else if imageSize = normalizeNanoBananaImageSize(request.Quality); imageSize != "" {
			grsReq.ImageSize = imageSize
		}
	} else {
		grsReq.AspectRatio = gptImageAspectRatio(request)
		if quality := normalizeImageQuality(request.Quality); quality != "" {
			grsReq.Quality = quality
		}
	}

	if info.PriceData.UsePrice {
		info.PriceData.AddOtherRatio("n", 1)
	}

	return grsReq, nil
}

func grsaiImageHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.Usage, *types.NewAPIError) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	service.CloseResponseBodyGracefully(resp)

	var submitResp submitResponse
	if err := common.Unmarshal(body, &submitResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if submitResp.Code != 0 || submitResp.Data.ID == "" {
		return nil, types.WithOpenAIError(types.OpenAIError{
			Message: firstNonEmpty(submitResp.Msg, "failed to submit image generation task"),
			Type:    "grsai_submit_error",
			Code:    fmt.Sprintf("%d", submitResp.Code),
		}, resp.StatusCode)
	}

	task, pollErr := pollTaskResult(info, submitResp.Data.ID)
	if pollErr != nil {
		recordGrsaiDrawingLog(info, imageReqFromRelayInfo(info), &taskResult{ID: submitResp.Data.ID, Status: "failed"}, nil, pollErr.Error())
		return nil, pollErr
	}
	if task.Status == "failed" {
		recordGrsaiDrawingLog(info, imageReqFromRelayInfo(info), task, nil, firstNonEmpty(task.Error, task.FailureReason))
		return nil, types.WithOpenAIError(types.OpenAIError{
			Message: firstNonEmpty(task.Error, task.FailureReason, "image generation failed"),
			Type:    "grsai_image_error",
			Code:    task.FailureReason,
		}, http.StatusBadRequest)
	}

	imageResponse := &dto.ImageResponse{
		Created: info.StartTime.Unix(),
		Data:    make([]dto.ImageData, 0, len(task.Results)),
	}
	for _, item := range task.Results {
		if strings.TrimSpace(item.URL) == "" {
			continue
		}
		imageResponse.Data = append(imageResponse.Data, dto.ImageData{
			Url:           item.URL,
			RevisedPrompt: item.Content,
		})
	}
	if len(imageResponse.Data) == 0 && strings.TrimSpace(task.URL) != "" {
		imageResponse.Data = append(imageResponse.Data, dto.ImageData{Url: task.URL})
	}
	if len(imageResponse.Data) == 0 {
		recordGrsaiDrawingLog(info, imageReqFromRelayInfo(info), task, nil, "no images generated")
		return nil, types.NewOpenAIError(fmt.Errorf("no images generated"), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	if info.PriceData.UsePrice {
		info.PriceData.AddOtherRatio("n", float64(len(imageResponse.Data)))
	}

	recordGrsaiDrawingLog(info, imageReqFromRelayInfo(info), task, imageResponse, "")

	responseBody, err := common.Marshal(imageResponse)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	if _, err := c.Writer.Write(responseBody); err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}

	return &dto.Usage{}, nil
}

func imageReqFromRelayInfo(info *relaycommon.RelayInfo) *dto.ImageRequest {
	if info == nil {
		return nil
	}
	if req, ok := info.Request.(*dto.ImageRequest); ok {
		return req
	}
	return nil
}

func recordGrsaiDrawingLog(info *relaycommon.RelayInfo, request *dto.ImageRequest, task *taskResult, imageResponse *dto.ImageResponse, failReason string) {
	if info == nil || task == nil || strings.TrimSpace(task.ID) == "" {
		return
	}

	now := time.Now().UnixMilli()
	progress := "100%"
	status := "SUCCESS"
	code := 1
	if strings.TrimSpace(failReason) != "" || strings.EqualFold(task.Status, "failed") {
		status = "FAILURE"
		code = 0
		if task.Progress > 0 {
			progress = fmt.Sprintf("%d%%", task.Progress)
		} else {
			progress = "0%"
		}
	}

	imageURL := strings.TrimSpace(task.URL)
	if imageResponse != nil && len(imageResponse.Data) > 0 && strings.TrimSpace(imageResponse.Data[0].Url) != "" {
		imageURL = strings.TrimSpace(imageResponse.Data[0].Url)
	}

	prompt := ""
	modelName := info.OriginModelName
	if request != nil {
		prompt = request.Prompt
		if modelName == "" {
			modelName = request.Model
		}
	}

	record := &model.Midjourney{
		Code:       code,
		UserId:     info.UserId,
		Action:     "IMAGINE",
		MjId:       task.ID,
		Prompt:     prompt,
		PromptEn:   prompt,
		State:      task.Status,
		SubmitTime: info.StartTime.UnixMilli(),
		StartTime:  info.StartTime.UnixMilli(),
		FinishTime: now,
		ImageUrl:   imageURL,
		Status:     status,
		Progress:   progress,
		FailReason: strings.TrimSpace(failReason),
		ChannelId:  info.ChannelId,
		Description: modelName,
	}

	if old := model.GetByOnlyMJId(task.ID); old != nil {
		old.Code = record.Code
		old.Action = record.Action
		old.Prompt = record.Prompt
		old.PromptEn = record.PromptEn
		old.State = record.State
		old.StartTime = record.StartTime
		old.FinishTime = record.FinishTime
		old.ImageUrl = record.ImageUrl
		old.Status = record.Status
		old.Progress = record.Progress
		old.FailReason = record.FailReason
		old.ChannelId = record.ChannelId
		old.Description = record.Description
		_ = old.Update()
		return
	}
	_ = record.Insert()
}

func pollTaskResult(info *relaycommon.RelayInfo, taskID string) (*taskResult, *types.NewAPIError) {
	client, err := service.GetHttpClientWithProxy(info.ChannelSetting.Proxy)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeDoRequestFailed)
	}

	baseURL := strings.TrimRight(info.ChannelBaseUrl, "/")
	queryURL := baseURL + "/v1/draw/result"
	requestBody, _ := common.Marshal(map[string]any{"id": taskID})

	const (
		maxAttempts         = 90
		perRequestTimeout   = 15 * time.Second
		pollInterval        = 2 * time.Second
	)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), perRequestTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, queryURL, bytes.NewBuffer(requestBody))
		if err != nil {
			cancel()
			return nil, types.NewError(err, types.ErrorCodeDoRequestFailed)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+info.ApiKey)

		resp, err := client.Do(req)
		cancel()
		if err != nil {
			if isRetryablePollError(err) {
				time.Sleep(pollInterval)
				continue
			}
			return nil, types.NewError(err, types.ErrorCodeDoRequestFailed)
		}

		body, readErr := io.ReadAll(resp.Body)
		service.CloseResponseBodyGracefully(resp)
		if readErr != nil {
			return nil, types.NewOpenAIError(readErr, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, types.WithOpenAIError(types.OpenAIError{
				Message: string(body),
				Type:    "grsai_result_error",
				Code:    fmt.Sprintf("%d", resp.StatusCode),
			}, resp.StatusCode)
		}

		var resultResp resultResponse
		if err := common.Unmarshal(body, &resultResp); err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		if resultResp.Code != 0 {
			return nil, types.WithOpenAIError(types.OpenAIError{
				Message: firstNonEmpty(resultResp.Msg, resultResp.Data.Error, "failed to query image task result"),
				Type:    "grsai_result_error",
				Code:    fmt.Sprintf("%d", resultResp.Code),
			}, http.StatusBadRequest)
		}

		switch resultResp.Data.Status {
		case "succeeded", "failed":
			return &resultResp.Data, nil
		}

		time.Sleep(pollInterval)
	}

	return nil, types.WithOpenAIError(types.OpenAIError{
		Message: "grsai image generation timed out",
		Type:    "grsai_timeout",
		Code:    "timeout",
	}, http.StatusGatewayTimeout)
}

func isRetryablePollError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func extractImageURLs(request dto.ImageRequest) []string {
	urls := extractStringSlice(request.Extra, "urls")
	if len(urls) > 0 {
		return urls
	}

	if len(request.Image) > 0 {
		var single string
		if err := common.Unmarshal(request.Image, &single); err == nil && strings.TrimSpace(single) != "" {
			return []string{single}
		}
		var multiple []string
		if err := common.Unmarshal(request.Image, &multiple); err == nil && len(multiple) > 0 {
			return multiple
		}
	}
	return nil
}

func gptImageAspectRatio(request dto.ImageRequest) string {
	if aspectRatio := extractString(request.Extra, "aspectRatio", "aspect_ratio"); aspectRatio != "" {
		return aspectRatio
	}
	size := strings.TrimSpace(request.Size)
	if size == "" {
		return "1:1"
	}
	if strings.Contains(size, ":") || strings.Contains(size, "x") {
		return size
	}
	return "1:1"
}

func nanoBananaAspectRatio(request dto.ImageRequest) string {
	if aspectRatio := extractString(request.Extra, "aspectRatio", "aspect_ratio"); aspectRatio != "" {
		return aspectRatio
	}

	switch strings.TrimSpace(request.Size) {
	case "", "auto":
		return "auto"
	case "1024x1024", "1:1":
		return "1:1"
	case "1792x1024", "16:9":
		return "16:9"
	case "1024x1792", "9:16":
		return "9:16"
	case "1536x1024", "3:2":
		return "3:2"
	case "1024x1536", "2:3":
		return "2:3"
	case "1152x864", "4:3":
		return "4:3"
	case "864x1152", "3:4":
		return "3:4"
	case "1280x1024", "5:4":
		return "5:4"
	case "1024x1280", "4:5":
		return "4:5"
	case "1344x576", "21:9":
		return "21:9"
	default:
		if strings.Contains(request.Size, ":") {
			return request.Size
		}
		return "auto"
	}
}

func normalizeImageQuality(quality string) string {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "":
		return ""
	case "auto", "standard":
		return "auto"
	case "hd", "high":
		return "high"
	case "medium":
		return "medium"
	case "low":
		return "low"
	default:
		return quality
	}
}

func normalizeNanoBananaImageSize(quality string) string {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "hd", "high", "2k":
		return "2K"
	case "4k":
		return "4K"
	case "", "auto", "standard", "medium", "low", "1k":
		return "1K"
	default:
		return ""
	}
}

func isNanoBananaModel(modelName string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(modelName)), "nano-banana")
}

func extractString(extra map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := extra[key]
		if !ok {
			continue
		}
		var value string
		if err := common.Unmarshal(raw, &value); err == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func extractStringSlice(extra map[string]json.RawMessage, keys ...string) []string {
	for _, key := range keys {
		raw, ok := extra[key]
		if !ok {
			continue
		}
		var values []string
		if err := common.Unmarshal(raw, &values); err == nil && len(values) > 0 {
			return values
		}
		var single string
		if err := common.Unmarshal(raw, &single); err == nil && strings.TrimSpace(single) != "" {
			return []string{strings.TrimSpace(single)}
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
