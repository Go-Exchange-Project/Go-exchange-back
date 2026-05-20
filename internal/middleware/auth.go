package middleware

import (
	"errors"
	"net/http"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/gin-gonic/gin"
)

func AuthRequired(tokenManager *auth.TokenManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if tokenManager == nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "auth token manager is not configured"})
			return
		}

		token, err := auth.ExtractBearerToken(c.GetHeader("Authorization"))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization bearer token is required"})
			return
		}

		userID, err := tokenManager.Parse(token)
		if err != nil {
			status := http.StatusUnauthorized
			message := "invalid authorization token"
			if errors.Is(err, auth.ErrExpiredToken) {
				message = "authorization token expired"
			}
			c.AbortWithStatusJSON(status, gin.H{"error": message})
			return
		}

		c.Set(auth.UserIDContextKey, userID)
		c.Next()
	}
}
