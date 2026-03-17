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
	log.Println("=== RESTful API Auto-Cache Adaptif (Tiga Pilar) ===")

	// ── Database ──────────────────────────────────────────────────────────
	dbPath := getEnv("DB_PATH", "./products.db")
	database, err := db.InitDB(dbPath)
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}
	defer database.Close()

	// ── Cache Adaptif ──────────────────────────────────────────────────────
	adaptiveCache := cache.NewAdaptiveCache()

	// ── Handler (mendaftarkan WarmupFunc ke cache) ─────────────────────────
	// NewProductHandler secara otomatis memanggil RegisterWarmupFunc,
	// sehingga WarmupScheduler tahu cara mengambil data dari DB.
	productHandler := handlers.NewProductHandler(database, adaptiveCache)

	// ── Goroutine untuk tugas background cache adaptif ─────────────────────
	go adaptiveCache.AutoCleanup()
	go adaptiveCache.MonthlyEvaluationCycle()
	go adaptiveCache.WarmupScheduler()

	// ── Gin Router ─────────────────────────────────────────────────────────
	if getEnv("GIN_MODE", "debug") == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(corsMiddleware())
	r.Use(requestTimingMiddleware())

	r.GET("/", func(c *gin.Context) {
		stats := adaptiveCache.GetStats()
		c.JSON(200, gin.H{
			"app":     "RESTful API Auto-Cache Adaptif",
			"version": "3.0.0",
			"tiga_pilar": gin.H{
				"pilar_1_akses_awal": gin.H{
					"fungsi": "Catat jam pertama key diakses tiap hari",
					"efek":   "Bulan depan, cache di-pre-warm otomatis pada jam yang sama",
					"contoh": "Jika products:all selalu pertama diakses pukul 06:30 → bulan depan auto-warm pukul 06:30",
				},
				"pilar_2_cache_hit": gin.H{
					"fungsi": "Hitung berapa kali setiap key di-hit",
					"efek":   "Hanya key dengan hit > 0 yang menjadi hot key bulan depan",
					"contoh": "products:all hit 500x, products:id:3 hit 2x → keduanya hot key bulan depan",
				},
				"pilar_3_durasi": gin.H{
					"fungsi": "Hitung durasi aktif key (LastAccess − FirstAccess) bulan ini",
					"efek":   "Durasi ini menjadi SuggestedTTL key tersebut di bulan berikutnya",
					"contoh": "products:all aktif dari 06:30 sampai 23:00 = 16.5 jam → TTL bulan depan 16.5 jam",
				},
			},
			"status": gin.H{
				"bulan_aktif":   stats.CurrentMonth,
				"item_di_cache": stats.ItemCount,
				"total_hits":    stats.TotalHits,
				"hit_ratio":     stats.HitRatioPct,
				"hot_keys":      len(stats.HotKeyProfiles),
				"ttl_baseline":  stats.TTLBaselineSec,
			},
			"endpoints": gin.H{
				"GET    /api/products":             "Ambil semua produk (cache-first)",
				"GET    /api/products/:id":         "Ambil produk by ID (cache-first)",
				"POST   /api/products":             "Tambah produk (invalidasi cache)",
				"PUT    /api/products/:id":         "Update produk (invalidasi cache)",
				"DELETE /api/products/:id":         "Hapus produk (invalidasi cache)",
				"GET    /api/cache/stats":          "Statistik + profil hot key + riwayat evaluasi",
				"POST   /api/cache/trigger-eval":   "Paksa evaluasi bulanan sekarang (testing)",
				"POST   /api/cache/trigger-warmup": "Paksa pre-warm semua hot key sekarang (testing)",
			},
		})
	})

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
			cacheGroup.POST("/trigger-warmup", productHandler.TriggerWarmup)
		}
	}

	port := getEnv("PORT", "8081")
	log.Printf("Server berjalan di http://localhost:%s", port)
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
		c.Header("X-Response-Time", time.Since(start).String())
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
