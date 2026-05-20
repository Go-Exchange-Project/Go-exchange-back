package handler

import (
	"net/http"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/gin-gonic/gin"
)

type DevHandler struct {
	DevWalletService *service.DevWalletService
}

type FundWalletRequest struct {
	CoinSymbol string `json:"coin_symbol" binding:"required"`
	Amount     string `json:"amount" binding:"required"`
}

func NewDevHandler(devWalletService *service.DevWalletService) *DevHandler {
	return &DevHandler{DevWalletService: devWalletService}
}

func (h *DevHandler) FundWallet(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authenticated user is required"})
		return
	}

	var req FundWalletRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	wallet, err := h.DevWalletService.FundWallet(service.FundWalletInput{
		UserID:     userID,
		CoinSymbol: req.CoinSymbol,
		Amount:     req.Amount,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "wallet funded",
		"wallet":  walletResponse(*wallet),
	})
}
