// Package handlers berisi HTTP handler untuk setiap endpoint API.
// Setiap handler menerapkan strategi cache-first:
//  1. Cek cache → jika HIT, kembalikan dari cache (TTL diperbarui adaptif)
//  2. Jika MISS, ambil dari database → simpan ke cache → kembalikan ke client
//
// Saat operasi CUD (Create/Update/Delete), cache terkait di-invalidasi
// untuk menjaga konsistensi data.
package handlers

import (
	"fmt"
	"net/http"
	"strconv"

	"smart-api/background/cache"
	"smart-api/background/db"
	"smart-api/background/models"

	"github.com/gin-gonic/gin"
)

// ── Konstanta Kunci Cache ──────────────────────────────────────────────────────

const (
	cacheKeyAllProducts = "products:all"
	cacheKeyProductByID = "products:id:%d"
)

// ── Handler ────────────────────────────────────────────────────────────────────

// ProductHandler menyatukan dependency database dan cache adaptif.
type ProductHandler struct {
	db    *db.DB
	cache *cache.AdaptiveCache
}

// NewProductHandler membuat ProductHandler baru.
func NewProductHandler(d *db.DB, c *cache.AdaptiveCache) *ProductHandler {
	return &ProductHandler{db: d, cache: c}
}

// ── GET /api/products ──────────────────────────────────────────────────────────

// GetAll mengambil seluruh produk.
// Prioritas: cache → database.
func (h *ProductHandler) GetAll(c *gin.Context) {
	// 1. Cek cache
	if cached, ok := h.cache.Get(cacheKeyAllProducts); ok {
		c.JSON(http.StatusOK, gin.H{
			"source":  "cache",
			"message": "Data diambil dari cache adaptif",
			"data":    cached,
		})
		return
	}

	// 2. Cache miss → query database
	products, err := h.db.GetAllProducts()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal mengambil data produk"})
		return
	}

	// 3. Simpan ke cache dengan TTL awal
	h.cache.Set(cacheKeyAllProducts, products)

	c.JSON(http.StatusOK, gin.H{
		"source":  "database",
		"message": "Data diambil dari database dan disimpan ke cache",
		"data":    products,
	})
}

// ── GET /api/products/:id ──────────────────────────────────────────────────────

// GetByID mengambil satu produk berdasarkan ID.
// Prioritas: cache → database.
func (h *ProductHandler) GetByID(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		return
	}

	cacheKey := fmt.Sprintf(cacheKeyProductByID, id)

	// 1. Cek cache
	if cached, ok := h.cache.Get(cacheKey); ok {
		c.JSON(http.StatusOK, gin.H{
			"source":  "cache",
			"message": "Data diambil dari cache adaptif",
			"data":    cached,
		})
		return
	}

	// 2. Cache miss → query database
	product, err := h.db.GetProductByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Produk dengan ID %d tidak ditemukan", id)})
		return
	}

	// 3. Simpan ke cache
	h.cache.Set(cacheKey, product)

	c.JSON(http.StatusOK, gin.H{
		"source":  "database",
		"message": "Data diambil dari database dan disimpan ke cache",
		"data":    product,
	})
}

// ── POST /api/products ─────────────────────────────────────────────────────────

// Create menambahkan produk baru dan menginvalidasi cache daftar produk.
func (h *ProductHandler) Create(c *gin.Context) {
	var req models.CreateProductRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Input tidak valid",
			"details": err.Error(),
		})
		return
	}

	product := &models.Product{
		Name:  req.Name,
		Price: req.Price,
	}

	if err := h.db.CreateProduct(product); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menyimpan produk baru"})
		return
	}

	// Invalidasi cache daftar produk (data berubah)
	h.cache.Delete(cacheKeyAllProducts)

	c.JSON(http.StatusCreated, gin.H{
		"message": "Produk berhasil ditambahkan",
		"data":    product,
	})
}

// ── PUT /api/products/:id ──────────────────────────────────────────────────────

// Update memperbarui produk berdasarkan ID dan menginvalidasi cache terkait.
func (h *ProductHandler) Update(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		return
	}

	// Cek apakah produk ada
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

	// Invalidasi cache daftar dan cache produk individual
	h.cache.Delete(cacheKeyAllProducts)
	h.cache.Delete(fmt.Sprintf(cacheKeyProductByID, id))

	c.JSON(http.StatusOK, gin.H{
		"message": "Produk berhasil diperbarui",
		"data": models.Product{
			ID:    id,
			Name:  req.Name,
			Price: req.Price,
		},
	})
}

// ── DELETE /api/products/:id ───────────────────────────────────────────────────

// Delete menghapus produk berdasarkan ID dan membersihkan cache terkait.
func (h *ProductHandler) Delete(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		return
	}

	// Cek apakah produk ada sebelum menghapus
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

	// Invalidasi cache terkait produk yang dihapus
	h.cache.Delete(cacheKeyAllProducts)
	h.cache.Delete(fmt.Sprintf(cacheKeyProductByID, id))

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Produk ID %d berhasil dihapus", id),
		"id":      id,
	})
}

// ── GET /api/cache/stats ───────────────────────────────────────────────────────

// CacheStats menampilkan statistik lengkap mekanisme auto-cache adaptif,
// termasuk status jendela waktu aktif dan klasifikasi frekuensi per-key.
func (h *ProductHandler) CacheStats(c *gin.Context) {
	stats := h.cache.GetStats()
	c.JSON(http.StatusOK, gin.H{
		"message":     "Statistik auto-cache adaptif",
		"cache_stats": stats,
		"description": map[string]string{
			"item_count":                 "Jumlah item aktif di cache saat ini",
			"total_hits":                 "Total request yang dilayani dari cache",
			"total_misses":               "Total request yang diteruskan ke database",
			"hit_ratio_pct":              "Persentase hit (makin tinggi = makin efisien)",
			"ttl_baseline_seconds":       "TTL_baseline global (berubah tiap siklus evaluasi)",
			"ttl_max_seconds":            "Batas maksimum TTL yang diizinkan",
			"adapt_coeff":                "Koefisien f(D): per detik D, TTL bertambah nilai ini",
			"active_window_start":        "Jam cache mulai aktif",
			"active_window_end":          "Jam cache berhenti aktif",
			"is_window_active_now":       "Apakah cache sedang aktif sekarang?",
			"window_status":              "Deskripsi status jendela aktif",
			"entries_cleaned_this_cycle": "Jumlah entry kadaluarsa dibersihkan siklus ini",
			"cycle_start_t0":             "Waktu awal siklus evaluasi (t0)",
			"last_eval_time":             "Waktu evaluasi siklus terakhir",
			"next_eval_time":             "Estimasi evaluasi siklus berikutnya",
			"key_stats":                  "Statistik per-key: hit, miss, class, TTL ditetapkan",
			"high_freq_keys":             "Key frekuensi TINGGI: TTL diperpanjang x3",
			"low_freq_keys":              "Key frekuensi RENDAH: TTL dipersingkat x0.5",
			"skipped_keys":               "Key yang diskip dari cache karena frekuensi sangat rendah",
		},
	})
}

// ── Helper ────────────────────────────────────────────────────────────────────

// parseID mem-parse parameter :id dari URL dan menulis error response jika gagal.
func parseID(c *gin.Context) (int64, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ID harus berupa angka positif"})
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}
