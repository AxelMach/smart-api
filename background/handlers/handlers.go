// Package handlers berisi HTTP handler untuk setiap endpoint API.
// Mendaftarkan WarmupFunc ke cache agar pre-warming otomatis dapat berjalan.
package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"smart-api/background/cache"
	"smart-api/background/db"
	"smart-api/background/models"

	"github.com/gin-gonic/gin"
)

const (
	cacheKeyAllProducts = "products:all"
	cacheKeyProductByID = "products:id:%d"
)

// ProductHandler menyatukan dependency database dan cache adaptif.
type ProductHandler struct {
	db    *db.DB
	cache *cache.AdaptiveCache
}

// NewProductHandler membuat ProductHandler dan mendaftarkan WarmupFunc.
// WarmupFunc ini yang akan dipanggil secara otomatis oleh WarmupScheduler
// saat jam pre-warm tiba (Pilar 1).
func NewProductHandler(d *db.DB, c *cache.AdaptiveCache) *ProductHandler {
	h := &ProductHandler{db: d, cache: c}

	// Daftarkan fungsi pre-warm ke cache.
	// Cache akan memanggil fungsi ini secara otomatis saat jam yang tepat.
	c.RegisterWarmupFunc(func(key string) (interface{}, error) {
		return h.fetchByKey(key)
	})

	return h
}

// fetchByKey mengambil data dari database berdasarkan cache key.
// Dipanggil oleh WarmupScheduler saat pre-warm otomatis.
func (h *ProductHandler) fetchByKey(key string) (interface{}, error) {
	if key == cacheKeyAllProducts {
		return h.db.GetAllProducts()
	}

	// Coba parse sebagai key by-ID
	var id int64
	_, err := fmt.Sscanf(key, "products:id:%d", &id)
	if err == nil && id > 0 {
		return h.db.GetProductByID(id)
	}

	return nil, fmt.Errorf("key tidak dikenali: %s", key)
}

// ── GET /api/products ────────────────────────────────────────────────────────

func (h *ProductHandler) GetAll(c *gin.Context) {
	if cached, ok := h.cache.Get(cacheKeyAllProducts); ok {
		c.JSON(http.StatusOK, gin.H{
			"source":  "cache",
			"message": "Data diambil dari cache adaptif",
			"data":    cached,
		})
		return
	}

	products, err := h.db.GetAllProducts()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal mengambil data produk"})
		return
	}

	h.cache.Set(cacheKeyAllProducts, products)

	c.JSON(http.StatusOK, gin.H{
		"source":  "database",
		"message": "Data diambil dari database dan disimpan ke cache",
		"data":    products,
	})
}

// ── GET /api/products/:id ────────────────────────────────────────────────────

func (h *ProductHandler) GetByID(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		return
	}

	cacheKey := fmt.Sprintf(cacheKeyProductByID, id)

	if cached, ok := h.cache.Get(cacheKey); ok {
		c.JSON(http.StatusOK, gin.H{
			"source":  "cache",
			"message": "Data diambil dari cache adaptif",
			"data":    cached,
		})
		return
	}

	product, err := h.db.GetProductByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Produk dengan ID %d tidak ditemukan", id),
		})
		return
	}

	h.cache.Set(cacheKey, product)

	c.JSON(http.StatusOK, gin.H{
		"source":  "database",
		"message": "Data diambil dari database dan disimpan ke cache",
		"data":    product,
	})
}

// ── POST /api/products ───────────────────────────────────────────────────────

func (h *ProductHandler) Create(c *gin.Context) {
	var req models.CreateProductRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Input tidak valid",
			"details": err.Error(),
		})
		return
	}

	product := &models.Product{Name: req.Name, Price: req.Price}
	if err := h.db.CreateProduct(product); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menyimpan produk baru"})
		return
	}

	h.cache.Delete(cacheKeyAllProducts)

	c.JSON(http.StatusCreated, gin.H{
		"message": "Produk berhasil ditambahkan",
		"data":    product,
	})
}

// ── PUT /api/products/:id ────────────────────────────────────────────────────

func (h *ProductHandler) Update(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		return
	}

	if !h.db.ProductExists(id) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Produk dengan ID %d tidak ditemukan", id),
		})
		return
	}

	var req models.UpdateProductRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Input tidak valid",
			"details": err.Error(),
		})
		return
	}

	if err := h.db.UpdateProduct(id, req.Name, req.Price); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal memperbarui produk"})
		return
	}

	h.cache.Delete(cacheKeyAllProducts)
	h.cache.Delete(fmt.Sprintf(cacheKeyProductByID, id))

	c.JSON(http.StatusOK, gin.H{
		"message": "Produk berhasil diperbarui",
		"data":    models.Product{ID: id, Name: req.Name, Price: req.Price},
	})
}

// ── DELETE /api/products/:id ─────────────────────────────────────────────────

func (h *ProductHandler) Delete(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		return
	}

	if !h.db.ProductExists(id) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Produk dengan ID %d tidak ditemukan", id),
		})
		return
	}

	if err := h.db.DeleteProduct(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menghapus produk"})
		return
	}

	h.cache.Delete(cacheKeyAllProducts)
	h.cache.Delete(fmt.Sprintf(cacheKeyProductByID, id))

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Produk ID %d berhasil dihapus", id),
		"id":      id,
	})
}

// ── GET /api/cache/stats ─────────────────────────────────────────────────────

func (h *ProductHandler) CacheStats(c *gin.Context) {
	stats := h.cache.GetStats()
	c.JSON(http.StatusOK, gin.H{
		"message":     "Statistik Auto-Cache Adaptif — Tiga Pilar",
		"pilar_1":     "Akses awal: jam pertama pengguna mengakses tiap hari → jadwal pre-warm bulan depan",
		"pilar_2":     "Cache hit: hanya key yang pernah di-hit yang menjadi hot key bulan depan",
		"pilar_3":     "Durasi cache: LastAccess − FirstAccess bulan ini → TTL key tersebut bulan depan",
		"cache_stats": stats,
	})
}

// ── POST /api/cache/trigger-eval ─────────────────────────────────────────────

func (h *ProductHandler) TriggerEval(c *gin.Context) {
	before := h.cache.GetStats()
	h.cache.TriggerEvaluationNow()
	after := h.cache.GetStats()

	c.JSON(http.StatusOK, gin.H{
		"message":          "Evaluasi bulanan dipicu secara manual",
		"hot_keys_sebelum": len(before.HotKeyProfiles),
		"hot_keys_sesudah": len(after.HotKeyProfiles),
		"hot_key_profiles": after.HotKeyProfiles,
		"triggered_at":     time.Now().Format(time.RFC3339),
	})
}

// ── POST /api/cache/trigger-warmup ───────────────────────────────────────────

// TriggerWarmup memaksa pre-warming semua hot key sekarang (untuk testing).
func (h *ProductHandler) TriggerWarmup(c *gin.Context) {
	h.cache.TriggerWarmupNow()
	stats := h.cache.GetStats()
	c.JSON(http.StatusOK, gin.H{
		"message":       "Pre-warming semua hot key dipicu secara manual",
		"item_in_cache": stats.ItemCount,
		"hot_keys":      stats.HotKeyProfiles,
		"triggered_at":  time.Now().Format(time.RFC3339),
	})
}

// ── Helper ────────────────────────────────────────────────────────────────────

func parseID(c *gin.Context) (int64, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ID harus berupa angka positif"})
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}
