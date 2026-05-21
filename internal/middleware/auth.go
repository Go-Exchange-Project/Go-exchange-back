package middleware

import (
	"errors"
	"net/http"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/httpapi"
	"github.com/gin-gonic/gin"
)

func AuthRequired(tokenManager *auth.TokenManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if tokenManager == nil {
			httpapi.AbortWithError(c, http.StatusInternalServerError, httpapi.CodeAuthNotConfigured, "auth token manager is not configured")
			return
		}

		token, err := auth.ExtractBearerToken(c.GetHeader("Authorization"))
		if err != nil {
			httpapi.AbortWithError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authorization bearer token is required")
			return
		}

		userID, err := tokenManager.Parse(token)
		if err != nil {
			message := "invalid authorization token"
			code := httpapi.CodeAuthInvalidToken
			if errors.Is(err, auth.ErrExpiredToken) {
				message = "authorization token expired"
				code = httpapi.CodeAuthExpiredToken
			}
			httpapi.AbortWithError(c, http.StatusUnauthorized, code, message)
			return
		}

		c.Set(auth.UserIDContextKey, userID)
		c.Next()
	}
}
