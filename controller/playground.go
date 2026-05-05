package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func playgroundRelay(c *gin.Context, relayFormat types.RelayFormat) {
	var newAPIError *types.NewAPIError

	defer func() {
		if newAPIError != nil {
			c.JSON(newAPIError.StatusCode, gin.H{
				"error": newAPIError.ToOpenAIError(),
			})
		}
	}()

	useAccessToken := c.GetBool("use_access_token")
	if useAccessToken {
		newAPIError = types.NewError(errors.New("暂不支持使用 access token"), types.ErrorCodeAccessDenied, types.ErrOptionWithSkipRetry())
		return
	}

	if relayFormat == types.RelayFormatOpenAI {
		relayFormat, newAPIError = normalizePlaygroundRelayFormat(c)
		if newAPIError != nil {
			return
		}
	}

	relayInfo, err := relaycommon.GenRelayInfo(c, relayFormat, nil, nil)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
		return
	}

	userId := c.GetInt("id")

	// Write user context to ensure acceptUnsetRatio is available
	userCache, err := model.GetUserCache(userId)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		return
	}
	userCache.WriteContext(c)

	tempToken := &model.Token{
		UserId: userId,
		Name:   fmt.Sprintf("playground-%s", relayInfo.UsingGroup),
		Group:  relayInfo.UsingGroup,
	}
	_ = middleware.SetupContextForToken(c, tempToken)

	Relay(c, relayFormat)
}

func Playground(c *gin.Context) {
	playgroundRelay(c, types.RelayFormatOpenAI)
}

func PlaygroundImage(c *gin.Context) {
	playgroundRelay(c, types.RelayFormatOpenAIImage)
}

func normalizePlaygroundRelayFormat(c *gin.Context) (types.RelayFormat, *types.NewAPIError) {
	if c.Request.URL.Path != "/pg/chat/completions" {
		return types.RelayFormatOpenAI, nil
	}

	var req dto.GeneralOpenAIRequest
	if err := common.UnmarshalBodyReusable(c, &req); err != nil {
		return types.RelayFormatOpenAI, types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	if !common.IsImageGenerationModel(req.Model) {
		return types.RelayFormatOpenAI, nil
	}

	imageReq := playgroundChatToImageRequest(req)
	if strings.TrimSpace(imageReq.Prompt) == "" {
		return types.RelayFormatOpenAI, nil
	}
	body, err := common.Marshal(imageReq)
	if err != nil {
		return types.RelayFormatOpenAI, types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	storage, err := common.CreateBodyStorage(body)
	if err != nil {
		return types.RelayFormatOpenAI, types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	if oldStorage, exists := c.Get(common.KeyBodyStorage); exists && oldStorage != nil {
		if old, ok := oldStorage.(common.BodyStorage); ok {
			old.Close()
		}
	}
	c.Set(common.KeyBodyStorage, storage)
	c.Request.Body = io.NopCloser(storage)
	c.Request.ContentLength = int64(len(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.URL.Path = "/pg/images/generations"
	c.Request.RequestURI = "/pg/images/generations"
	c.Set("relay_mode", relayconstant.RelayModeImagesGenerations)
	return types.RelayFormatOpenAIImage, nil
}

func playgroundChatToImageRequest(req dto.GeneralOpenAIRequest) dto.ImageRequest {
	imageReq := dto.ImageRequest{
		Model:  req.Model,
		Prompt: extractPlaygroundPrompt(req.Messages),
		Extra:  map[string]json.RawMessage{},
	}
	if req.Size != "" {
		imageReq.Size = req.Size
	}
	imageURLs := extractPlaygroundImageURLs(req.Messages)
	if len(imageURLs) > 0 {
		imageReq.Extra["urls"], _ = common.Marshal(imageURLs)
	}
	return imageReq
}

func extractPlaygroundPrompt(messages []dto.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		content := strings.TrimSpace(messages[i].StringContent())
		if content != "" {
			return content
		}
		for _, part := range messages[i].ParseContent() {
			if part.Type == dto.ContentTypeText {
				content = strings.TrimSpace(part.Text)
				if content != "" {
					return content
				}
			}
		}
	}
	return ""
}

func extractPlaygroundImageURLs(messages []dto.Message) []string {
	var urls []string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		for _, part := range messages[i].ParseContent() {
			if part.Type != dto.ContentTypeImageURL {
				continue
			}
			if img := part.GetImageMedia(); img != nil && strings.TrimSpace(img.Url) != "" {
				urls = append(urls, strings.TrimSpace(img.Url))
			}
		}
		if len(urls) > 0 {
			break
		}
	}
	return urls
}
