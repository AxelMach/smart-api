// ╔══════════════════════════════════════════════════════════════════════════╗
// ║  RESTFUL API PINTAR - AUTO-CACHE ADAPTIF                                ║
// ║  Golang + Gin Framework + SQLite                                         ║
// ║  Oleh: Dita Aulia Al Farid (2109020178)                                  ║
// ║  Universitas Muhammadiyah Sumatera Utara - 2026                          ║
// ╚══════════════════════════════════════════════════════════════════════════╝
//
// Alur sistem (sesuai rancangan Bab III):
//
//  Client Request
//      │
//      ▼
//  [Gin Router] ──► [Handler]
//                       │
//                 ┌─────▼──────┐
//                 │ Cache Check │
//                 └─────┬──────┘
//           HIT ◄───────┴──────► MISS
//            │                    │
//     Hitung D = t_now - T0   Query DB
//     TTL_runtime = baseline    Store to Cache
//        + f(D)                  (TTL = baseline)
//     Update t_expire             │
//            └──────────┬─────────┘
//                       ▼
//                   JSON Response

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
	// ── Konfigurasi logging ────────────────────────────────────────────────
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.SetPrefix("[SmartAPI] ")
	log.Println("=== RESTful API Pintar dengan Auto-Cache Adaptif ===")
	log.Println("Menggunakan: Golang + Gin Framework + SQLite")
	log.Println("Algoritma: TTL_runtime = TTL_baseline + f(D), D = t_now - T0")

	// ── Inisialisasi Database ──────────────────────────────────────────────
	dbPath := getEnv("DB_PATH", "./products.db")
	database, err := db.InitDB(dbPath)
	if err != nil {
		log.Fatalf("FATAL: Gagal menginisialisasi database: %v", err)
	}
	defer database.Close()

	// ── Inisialisasi Cache Adaptif ─────────────────────────────────────────
	adaptiveCache := cache.NewAdaptiveCache()

	// Jalankan goroutine background:
	//   1. AutoCleanup  → hapus entry kadaluarsa setiap 1 menit
	//   2. EvaluationCycle → re-kalibrasi TTL_baseline setiap 30 hari
	go adaptiveCache.AutoCleanup()
	go adaptiveCache.EvaluationCycle()

	// ── Inisialisasi Handler ───────────────────────────────────────────────
	productHandler := handlers.NewProductHandler(database, adaptiveCache)

	// ── Setup Gin Router ───────────────────────────────────────────────────
	if getEnv("GIN_MODE", "debug") == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()

	// Middleware
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(corsMiddleware())
	r.Use(requestTimingMiddleware())

	// ── Endpoint Root: Info Sistem ─────────────────────────────────────────
	r.GET("/", func(c *gin.Context) {
		cacheStats := adaptiveCache.GetStats()
		c.JSON(200, gin.H{
			"app":          "RESTful API Pintar dengan Auto-Cache Adaptif",
			"version":      "1.0.0",
			"framework":    "Gin v1.9.1",
			"database":     "SQLite (modernc.org/sqlite)",
			"cache_engine": "Custom In-Memory Adaptive Cache",
			"algorithm": gin.H{
				"formula":      "TTL_runtime = TTL_baseline + AdaptCoeff × D",
				"D":            "Durasi entry di cache (detik)",
				"ttl_baseline": cacheStats.TTLBaselineSec,
				"ttl_max":      cacheStats.TTLMaxSec,
				"adapt_coeff":  cacheStats.AdaptCoeff,
				"eval_cycle":   "30 hari",
			},
			"endpoints": gin.H{
				"GET    /api/products":     "Ambil semua produk (cache-first)",
				"GET    /api/products/:id": "Ambil produk by ID (cache-first)",
				"POST   /api/products":     "Tambah produk baru (invalidasi cache)",
				"PUT    /api/products/:id": "Update produk (invalidasi cache)",
				"DELETE /api/products/:id": "Hapus produk (invalidasi cache)",
				"GET    /api/cache/stats":  "Statistik auto-cache adaptif",
			},
		})
	})

	// ── Grup Endpoint API ──────────────────────────────────────────────────
	api := r.Group("/api")
	{
		// Endpoint Produk (CRUD + Cache Adaptif)
		products := api.Group("/products")
		{
			products.GET("", productHandler.GetAll)
			products.GET("/:id", productHandler.GetByID)
			products.POST("", productHandler.Create)
			products.PUT("/:id", productHandler.Update)
			products.DELETE("/:id", productHandler.Delete)
		}

		// Endpoint Statistik Cache
		api.GET("/cache/stats", productHandler.CacheStats)
	}

	// ── Jalankan Server ────────────────────────────────────────────────────
	port := getEnv("PORT", "8080")
	log.Printf("Server berjalan di http://localhost:%s", port)
	log.Printf("Tekan Ctrl+C untuk menghentikan server.")
	log.Println("─────────────────────────────────────────────────")

	if err := r.Run(":" + port); err != nil {
		log.Fatalf("FATAL: Gagal menjalankan server: %v", err)
	}
}

// ── Middleware ─────────────────────────────────────────────────────────────────

// corsMiddleware menambahkan header CORS agar API dapat diakses dari browser.
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

// requestTimingMiddleware mencatat waktu pemrosesan setiap request.
func requestTimingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		duration := time.Since(start)
		c.Header("X-Response-Time", duration.String())
		log.Printf("%-6s %-30s → %d [%v]",
			c.Request.Method,
			c.Request.URL.Path,
			c.Writer.Status(),
			duration,
		)
	}
}

// ── Utility ────────────────────────────────────────────────────────────────────

// getEnv mengambil nilai environment variable, atau fallback ke defaultVal.
func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
