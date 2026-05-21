package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/httpapi"
	"github.com/gin-gonic/gin"
)

const DevToolsTokenHeader = "X-GoExchange-Dev-Token"

func DevToolsRequired(expectedToken string) gin.HandlerFunc {
	expectedToken = strings.TrimSpace(expectedToken)

	return func(c *gin.Context) {
		if expectedToken == "" {
			httpapi.AbortWithError(c, http.StatusNotFound, httpapi.CodeDevToolsDisabled, "development tools are disabled")
			return
		}

		providedToken := strings.TrimSpace(c.GetHeader(DevToolsTokenHeader))
		if providedToken == "" || subtle.ConstantTimeCompare([]byte(providedToken), []byte(expectedToken)) != 1 {
			httpapi.AbortWithError(c, http.StatusForbidden, httpapi.CodeDevToolsForbidden, "development tool token is invalid")
			return
		}

		c.Next()
	}
}
