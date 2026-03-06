// ╔══════════════════════════════════════════════════════════════════════════╗
// ║  RESTFUL API — AUTO-CACHE ADAPTIF (EVALUASI PERGANTIAN BULAN)           ║
// ║  Golang + Gin Framework + SQLite                                         ║
// ║                                                                          ║
// ║  Algoritma:                                                              ║
// ║    TTL_runtime = TTL_baseline + AdaptCoeff × (TLast − T0)               ║
// ║    Saat bulan berganti:                                                  ║
// ║      D_avg = rata-rata (TLast−T0) semua entry bulan lalu                ║
// ║      TTL_baseline_baru = TTL_baseline_lama + AdaptCoeff × D_avg         ║
// ╚══════════════════════════════════════════════════════════════════════════╝
package main

import (
	"log"
	"os"
	"time"

	"smart-api/background/cache"
	"smart-api/background/db"
	"smart-api/background/handlers"

	"github.com/gin-gonic/gin"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.SetPrefix("[API] ")
	log.Println("=== RESTful API dengan Auto-Cache Adaptif (Evaluasi per Bulan) ===")

	// ── Database ─────────────────────────────────────────────────────────
	dbPath := getEnv("DB_PATH", "./products.db")
	database, err := db.InitDB(dbPath)
	if err != nil {
		log.Fatalf("FATAL: Gagal inisialisasi database: %v", err)
	}
	defer database.Close()

	// ── Cache Adaptif ─────────────────────────────────────────────────────
	adaptiveCache := cache.NewAdaptiveCache()

	// Goroutine background:
	//   1. AutoCleanup          → hapus entry kadaluarsa setiap 1 menit
	//   2. MonthlyEvaluationCycle → re-kalibrasi TTL_baseline tiap pergantian bulan
	go adaptiveCache.AutoCleanup()
	go adaptiveCache.MonthlyEvaluationCycle()

	// ── Handler & Router ─────────────────────────────────────────────────
	productHandler := handlers.NewProductHandler(database, adaptiveCache)

	if getEnv("GIN_MODE", "debug") == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(corsMiddleware())
	r.Use(requestTimingMiddleware())

	// Root: info sistem
	r.GET("/", func(c *gin.Context) {
		stats := adaptiveCache.GetStats()
		c.JSON(200, gin.H{
			"app":       "RESTful API dengan Auto-Cache Adaptif",
			"version":   "2.0.0",
			"framework": "Gin v1.9.1",
			"database":  "SQLite",
			"cache": gin.H{
				"type":          "In-Memory Adaptive Cache",
				"ttl_baseline":  stats.TTLBaselineSec,
				"ttl_max":       stats.TTLMaxSec,
				"adapt_coeff":   stats.AdaptCoeff,
				"eval_trigger":  "Setiap pergantian bulan",
				"current_month": stats.CurrentMonth,
				"next_eval":     stats.NextEvalEstimate,
			},
			"endpoints": gin.H{
				"GET    /api/products":           "Ambil semua produk (cache-first)",
				"GET    /api/products/:id":       "Ambil produk by ID (cache-first)",
				"POST   /api/products":           "Tambah produk (invalidasi cache)",
				"PUT    /api/products/:id":       "Update produk (invalidasi cache)",
				"DELETE /api/products/:id":       "Hapus produk (invalidasi cache)",
				"GET    /api/cache/stats":        "Statistik cache adaptif + riwayat bulanan",
				"POST   /api/cache/trigger-eval": "Paksa evaluasi bulanan sekarang (untuk testing)",
			},
		})
	})

	// Grup /api
	api := r.Group("/api")
	{
		products := api.Group("/products")
		{
			products.GET("", productHandler.GetAll)
			products.GET("/:id", productHandler.GetByID)
			products.POST("", productHandler.Create)
			products.PUT("/:id", productHandler.Update)
			products.DELETE("/:id", productHandler.Delete)
		}

		cacheGroup := api.Group("/cache")
		{
			cacheGroup.GET("/stats", productHandler.CacheStats)
			cacheGroup.POST("/trigger-eval", productHandler.TriggerEval)
		}
	}

	// Jalankan server
	port := getEnv("PORT", "8081")
	log.Printf("Server berjalan di http://localhost:%s", port)
	log.Printf("Evaluasi TTL_baseline akan dijalankan otomatis setiap pergantian bulan.")
	log.Println("Gunakan POST /api/cache/trigger-eval untuk simulasi evaluasi sekarang.")
	log.Println("────────────────────────────────────────────────────────────────────")

	if err := r.Run(":" + port); err != nil {
		log.Fatalf("FATAL: %v", err)
	}
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

func requestTimingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		duration := time.Since(start)
		c.Header("X-Response-Time", duration.String())
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
