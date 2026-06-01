package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	CodeAuthRequired       = "AUTH_REQUIRED"
	CodeAuthInvalidToken   = "AUTH_INVALID_TOKEN"
	CodeAuthExpiredToken   = "AUTH_EXPIRED_TOKEN"
	CodeAuthNotConfigured  = "AUTH_NOT_CONFIGURED"
	CodeBadRequest         = "BAD_REQUEST"
	CodeConflict           = "CONFLICT"
	CodeDevToolsDisabled   = "DEV_TOOLS_DISABLED"
	CodeDevToolsForbidden  = "DEV_TOOLS_FORBIDDEN"
	CodeForbidden          = "FORBIDDEN"
	CodeInternal           = "INTERNAL_ERROR"
	CodeNotFound           = "NOT_FOUND"
	CodeValidation         = "VALIDATION_ERROR"
	CodeInvalidCredentials = "INVALID_CREDENTIALS"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ErrorResponse struct {
	Error Error `json:"error"`
}

type DataResponse struct {
	Data interface{} `json:"data"`
}

func WriteData(c *gin.Context, status int, data interface{}) {
	c.JSON(status, DataResponse{Data: data})
}

func WriteError(c *gin.Context, status int, code string, message string) {
	c.JSON(status, ErrorResponse{Error: Error{
		Code:    normalizeCode(code),
		Message: normalizeMessage(message),
	}})
}

func AbortWithError(c *gin.Context, status int, code string, message string) {
	c.AbortWithStatusJSON(status, ErrorResponse{Error: Error{
		Code:    normalizeCode(code),
		Message: normalizeMessage(message),
	}})
}

func CodeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return CodeAuthRequired
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeConflict
	case http.StatusUnprocessableEntity:
		return CodeValidation
	case http.StatusInternalServerError:
		return CodeInternal
	default:
		return CodeBadRequest
	}
}

func normalizeCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return CodeBadRequest
	}
	return code
}

func normalizeMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return "request failed"
	}
	return message
}
