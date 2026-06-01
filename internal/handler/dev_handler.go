package handler

import (
	"net/http"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/httpapi"
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
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	var req FundWalletRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeBindingError(c, err)
		return
	}

	wallet, err := h.DevWalletService.FundWallet(service.FundWalletInput{
		UserID:     userID,
		CoinSymbol: req.CoinSymbol,
		Amount:     req.Amount,
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}

	httpapi.WriteData(c, http.StatusOK, gin.H{
		"message": "wallet funded",
		"wallet":  walletResponse(*wallet),
	})
}
