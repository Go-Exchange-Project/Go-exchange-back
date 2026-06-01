package handler

import (
	"net/http"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/httpapi"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/gin-gonic/gin"
)

type AuthHandler struct {
	AuthService *service.AuthService
}

type RegisterRequest struct {
	Name     string `json:"name" binding:"required"`
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type LoginRequest struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

func NewAuthHandler(authService *service.AuthService) *AuthHandler {
	return &AuthHandler{AuthService: authService}
}

func (h *AuthHandler) Register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeBindingError(c, err)
		return
	}

	result, err := h.AuthService.Register(service.RegisterInput{
		Name:     req.Name,
		Email:    req.Email,
		Password: req.Password,
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}

	httpapi.WriteData(c, http.StatusCreated, authResponse(result))
}

func (h *AuthHandler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeBindingError(c, err)
		return
	}

	result, err := h.AuthService.Login(service.LoginInput{
		Email:    req.Email,
		Password: req.Password,
	})
	if err != nil {
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeInvalidCredentials, err.Error())
		return
	}

	httpapi.WriteData(c, http.StatusOK, authResponse(result))
}

func authResponse(result service.AuthResult) gin.H {
	return gin.H{
		"token": result.Token,
		"user": gin.H{
			"id":    result.User.ID,
			"name":  result.User.Name,
			"email": result.User.Email,
		},
	}
}
