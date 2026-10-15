package main

import (
	"net/http"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/handler"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/httpapi"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/middleware"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/ws"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// routerConfig는 main()이 조립한 의존성으로 서비스 라우터를 구성하는 데
// 필요한 전부다. main()과 라우터 수준 테스트(Task 8 Step 3) 양쪽에서 실제
// 배선을 공유하기 위해 newRouter로 뺐다 — 단위 테스트만으로는 배선이 틀려도
// 통과하므로, 실제 gin.Engine으로 검증해야 한다.
type routerConfig struct {
	corsOrigins      []string
	trustedProxies   []string
	hub              *ws.Hub
	authHandler      *handler.AuthHandler
	marketHandler    *handler.MarketHandler
	orderBookHandler *handler.OrderBookHandler
	orderHandler     *handler.OrderHandler
	transferHandler  *handler.TransferHandler
	tokenManager     *auth.TokenManager

	devToolsEnabled  bool
	devWalletService *service.DevWalletService
	devToolsToken    string

	authRateLimiter     *middleware.RateLimiter
	orderRateLimiter    *middleware.RateLimiter
	transferRateLimiter *middleware.RateLimiter
}

// newRouter는 main()의 서비스 라우터 배선을 그대로 구성한다(§7 — /metrics·
// pprof는 관리 서버 쪽이라 여기 없다). 인증 limiter는 /auth/login·/auth/register
// 핸들러 앞(IP 키, 경로별 별도 버킷), 사용자 limiter는 AuthRequired 뒤(설계
// §6.2)에 배치한다.
func newRouter(cfg routerConfig) (*gin.Engine, error) {
	r := gin.Default()
	if err := r.SetTrustedProxies(cfg.trustedProxies); err != nil {
		return nil, err
	}

	r.Use(cors.New(cors.Config{
		AllowOrigins: cfg.corsOrigins,
		AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders: corsAllowedHeaders,
	}))
	r.Use(metrics.HTTPMiddleware())

	r.GET("/ping", func(c *gin.Context) {
		httpapi.WriteData(c, http.StatusOK, gin.H{
			"message": "pong",
		})
	})

	r.GET("/ws", func(c *gin.Context) {
		ws.ServeWs(cfg.hub, c)
	})

	r.POST("/auth/register", middleware.RateLimitByIP(cfg.authRateLimiter, "register"), cfg.authHandler.Register)
	r.POST("/auth/login", middleware.RateLimitByIP(cfg.authRateLimiter, "login"), cfg.authHandler.Login)
	r.GET("/markets/rules", cfg.marketHandler.GetRules)
	r.GET("/orderbook", cfg.orderBookHandler.GetSnapshot)

	authenticated := r.Group("/")
	authenticated.Use(middleware.AuthRequired(cfg.tokenManager))
	authenticated.GET("/orders", cfg.orderHandler.ListOrders)
	authenticated.GET("/orders/:id", cfg.orderHandler.GetOrder)
	authenticated.POST("/orders", middleware.RateLimitByUser(cfg.orderRateLimiter), cfg.orderHandler.CreateOrder)
	authenticated.DELETE("/orders/:id", middleware.RateLimitByUser(cfg.orderRateLimiter), cfg.orderHandler.CancelOrder)
	authenticated.GET("/wallets", cfg.orderHandler.ListWallets)
	authenticated.GET("/trades", cfg.orderHandler.ListTrades)
	authenticated.POST("/transfers/deposits", middleware.RateLimitByUser(cfg.transferRateLimiter), cfg.transferHandler.RequestDeposit)
	authenticated.POST("/transfers/withdrawals", middleware.RateLimitByUser(cfg.transferRateLimiter), cfg.transferHandler.RequestWithdrawal)
	authenticated.GET("/transfers", cfg.transferHandler.ListTransfers)
	if cfg.devToolsEnabled {
		devHandler := handler.NewDevHandler(cfg.devWalletService)
		dev := authenticated.Group("/dev")
		dev.Use(middleware.DevToolsRequired(cfg.devToolsToken))
		dev.POST("/wallets/fund", devHandler.FundWallet)
		// 가짜 은행·가짜 체인이 우리에게 알림을 보내는 것을 흉내 낸다 — 실제
		// 외부가 호출하는 라우트가 아니므로 dev-tools 뒤에 둔다.
		dev.POST("/transfers/callback", cfg.transferHandler.ReceiveCallback)
	}

	return r, nil
}
