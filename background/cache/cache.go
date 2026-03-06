// Package cache mengimplementasikan mekanisme Auto-Cache Adaptif
// berbasis durasi akses sesuai rancangan penelitian.
//
// Algoritma inti:
//
//	TTL_runtime = TTL_baseline + f(D)
//	D           = t_now - entry.T0  (durasi entry telah tersimpan di cache)
//	f(D)        = AdaptCoeff * D    (fungsi proporsional linier)
//
// Setiap 30 hari sistem melakukan evaluasi siklus:
//
//	D_total          = t_akhir_siklus - t₀
//	D_avg            = D_total / 30
//	TTL_baseline_baru = TTL_baseline_lama + g(D_avg)
//	g(D_avg)         = AdaptCoeff * D_avg
//
// TTL selalu dibatasi oleh TTL_max.
package cache

import (
	"log"
	"strings"
	"sync"
	"time"
)

// ── Konstanta default ──────────────────────────────────────────────────────────

const (
	// TTL awal cache saat sistem baru berjalan
	DefaultTTLBaseline = 5 * time.Minute

	// Batas maksimum TTL yang diizinkan
	DefaultTTLMax = 1 * time.Hour

	// Koefisien adaptasi: setiap detik entry tersimpan di cache,
	// TTL bertambah 0.001 detik. Contoh: entry tersimpan 1 jam (3600 dtk)
	// → TTL bertambah 3.6 detik.
	DefaultAdaptCoeff = 0.001

	// Interval evaluasi siklus 30 hari
	EvalCycleInterval = 30 * 24 * time.Hour

	// Interval pembersihan otomatis entry kadaluarsa
	CleanupInterval = 1 * time.Minute
)

// ── Struktur data ──────────────────────────────────────────────────────────────

// CacheEntry menyimpan satu item dalam cache beserta metadata temporalnya.
type CacheEntry struct {
	Value    interface{} // data yang di-cache
	ExpireAt time.Time   // waktu kedaluwarsa (t_expire)
	T0       time.Time   // waktu entry pertama kali disimpan di cache
	HitCount int64       // jumlah kali entry ini di-hit
}

// CacheStats berisi statistik kinerja cache adaptif.
type CacheStats struct {
	ItemCount       int       `json:"item_count"`
	TotalHits       int64     `json:"total_hits"`
	TotalMisses     int64     `json:"total_misses"`
	HitRatio        float64   `json:"hit_ratio_pct"`
	AvgDurationSec  float64   `json:"avg_entry_duration_seconds"`
	AvgActiveTTLSec float64   `json:"avg_active_ttl_seconds"`
	TTLBaselineSec  float64   `json:"ttl_baseline_seconds"`
	TTLMaxSec       float64   `json:"ttl_max_seconds"`
	AdaptCoeff      float64   `json:"adapt_coeff"`
	CleanupCount    int64     `json:"entries_cleaned_this_cycle"`
	CycleStart      time.Time `json:"cycle_start_t0"`
	LastEvalTime    time.Time `json:"last_eval_time"`
	NextEvalTime    time.Time `json:"next_eval_time"`
}

// AdaptiveCache adalah in-memory cache dengan TTL dinamis berbasis durasi.
type AdaptiveCache struct {
	mu    sync.RWMutex
	items map[string]*CacheEntry

	// Parameter adaptasi
	TTLBaseline time.Duration
	TTLMax      time.Duration
	AdaptCoeff  float64 // koefisien f(D) dan g(D_avg)

	// Metadata siklus 30 hari
	CycleStart time.Time // t₀ awal siklus evaluasi
	LastEval   time.Time
	NextEval   time.Time

	// Statistik global
	TotalHits    int64
	TotalMisses  int64
	CleanupCount int64 // jumlah entry dibersihkan dalam siklus ini
}

// ── Konstruktor ────────────────────────────────────────────────────────────────

// NewAdaptiveCache membuat instance cache adaptif dengan nilai default.
func NewAdaptiveCache() *AdaptiveCache {
	now := time.Now()
	c := &AdaptiveCache{
		items:       make(map[string]*CacheEntry),
		TTLBaseline: DefaultTTLBaseline,
		TTLMax:      DefaultTTLMax,
		AdaptCoeff:  DefaultAdaptCoeff,
		CycleStart:  now,
		LastEval:    now,
		NextEval:    now.Add(EvalCycleInterval),
	}
	log.Printf("[AdaptiveCache] Init → TTL_baseline=%v TTL_max=%v AdaptCoeff=%.4f",
		c.TTLBaseline, c.TTLMax, c.AdaptCoeff)
	return c
}

// ── Perhitungan TTL Adaptif ────────────────────────────────────────────────────

// computeRuntimeTTL menghitung TTL_runtime berdasarkan durasi entry telah
// tersimpan di cache (D = t_now - entry.T0).
//
//	TTL_runtime = TTL_baseline + f(D)
//	f(D)        = AdaptCoeff * D.Seconds()
//
// Hasil dibatasi oleh TTL_max.
func (c *AdaptiveCache) computeRuntimeTTL(entry *CacheEntry) time.Duration {
	D := time.Since(entry.T0) // durasi entry di cache

	baselineSec := c.TTLBaseline.Seconds()
	adaptedSec := baselineSec + c.AdaptCoeff*D.Seconds()

	runtimeTTL := time.Duration(adaptedSec * float64(time.Second))

	// Validasi batas maksimum TTL
	if runtimeTTL > c.TTLMax {
		runtimeTTL = c.TTLMax
	}

	return runtimeTTL
}

// ── Operasi Cache Dasar ────────────────────────────────────────────────────────

// Get mengambil item dari cache. Mengembalikan (value, true) jika hit,
// (nil, false) jika miss atau entry sudah kadaluarsa.
//
// Saat HIT: TTL entry diperbarui secara adaptif sebelum dikembalikan.
func (c *AdaptiveCache) Get(key string) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, exists := c.items[key]
	if !exists {
		c.TotalMisses++
		return nil, false
	}

	// Cek apakah entry sudah kadaluarsa
	if time.Now().After(entry.ExpireAt) {
		delete(c.items, key)
		c.CleanupCount++
		c.TotalMisses++
		return nil, false
	}

	// ── Cache HIT ──
	c.TotalHits++
	entry.HitCount++

	// Hitung ulang TTL adaptif berdasarkan durasi entry (sesuai 3.5.1.c-d)
	newTTL := c.computeRuntimeTTL(entry)
	entry.ExpireAt = time.Now().Add(newTTL) // perbarui t_expire

	log.Printf("[Cache HIT] key=%q D=%.1fs TTL_runtime=%v new_expire=%v",
		key,
		time.Since(entry.T0).Seconds(),
		newTTL.Round(time.Second),
		entry.ExpireAt.Format("15:04:05"),
	)

	return entry.Value, true
}

// Set menyimpan item ke cache dengan TTL awal (TTL_baseline).
// Jika key sudah ada, data diperbarui dan TTL dihitung ulang dari T0 asli.
func (c *AdaptiveCache) Set(key string, value interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	// Jika entry sudah ada → perbarui value, pertahankan T0 lama
	if existing, ok := c.items[key]; ok {
		existing.Value = value
		newTTL := c.computeRuntimeTTL(existing)
		existing.ExpireAt = now.Add(newTTL)
		log.Printf("[Cache SET-UPDATE] key=%q TTL_runtime=%v", key, newTTL.Round(time.Second))
		return
	}

	// Entry baru → set T0 = now, gunakan TTL_baseline sebagai TTL awal
	entry := &CacheEntry{
		Value:    value,
		T0:       now,
		ExpireAt: now.Add(c.TTLBaseline),
		HitCount: 0,
	}
	c.items[key] = entry
	log.Printf("[Cache SET-NEW] key=%q TTL_baseline=%v expire=%v",
		key, c.TTLBaseline, entry.ExpireAt.Format("15:04:05"))
}

// Delete menghapus satu item dari cache (digunakan saat invalidasi CRUD).
func (c *AdaptiveCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; ok {
		delete(c.items, key)
		log.Printf("[Cache INVALIDATE] key=%q dihapus", key)
	}
}

// DeleteByPrefix menghapus semua item yang kuncinya diawali prefix tertentu.
func (c *AdaptiveCache) DeleteByPrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for k := range c.items {
		if strings.HasPrefix(k, prefix) {
			delete(c.items, k)
			count++
		}
	}
	if count > 0 {
		log.Printf("[Cache INVALIDATE-PREFIX] prefix=%q → %d item dihapus", prefix, count)
	}
}

// ── Goroutine Background ───────────────────────────────────────────────────────

// AutoCleanup menjalankan proses pembersihan entry kadaluarsa secara periodik.
// Panggil sebagai goroutine: go cache.AutoCleanup()
func (c *AdaptiveCache) AutoCleanup() {
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()
	log.Println("[Cache] AutoCleanup goroutine dimulai (interval:", CleanupInterval, ")")

	for range ticker.C {
		c.runCleanup()
	}
}

func (c *AdaptiveCache) runCleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	removed := 0
	for k, entry := range c.items {
		// Aturan penghapusan: t_now >= t_expire
		if now.After(entry.ExpireAt) {
			delete(c.items, k)
			c.CleanupCount++
			removed++
		}
	}
	if removed > 0 {
		log.Printf("[Cache Cleanup] %d entry kadaluarsa dihapus | Total siklus ini: %d",
			removed, c.CleanupCount)
	}
}

// EvaluationCycle menjalankan siklus evaluasi 30 hari untuk re-kalibrasi
// TTL_baseline berdasarkan performa operasional.
// Panggil sebagai goroutine: go cache.EvaluationCycle()
func (c *AdaptiveCache) EvaluationCycle() {
	ticker := time.NewTicker(EvalCycleInterval)
	defer ticker.Stop()
	log.Println("[Cache] EvaluationCycle goroutine dimulai (interval:", EvalCycleInterval, ")")

	for range ticker.C {
		c.runEvaluation()
	}
}

// runEvaluation menjalankan satu siklus evaluasi 30 hari:
//
//  1. Hitung D_total = t_akhir_siklus - t₀
//  2. Hitung D_avg   = D_total / 30
//  3. Perbarui TTL_baseline: TTL_baseline += g(D_avg)
//  4. Reset metadata temporal
func (c *AdaptiveCache) runEvaluation() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	// 1. Hitung total durasi siklus
	D_total := now.Sub(c.CycleStart)

	// 2. Rata-rata harian
	D_avg := D_total / 30

	// 3. g(D_avg) = AdaptCoeff * D_avg → tambahkan ke TTL_baseline
	g_Davg := time.Duration(c.AdaptCoeff * float64(D_avg))
	newBaseline := c.TTLBaseline + g_Davg

	// Batasi oleh TTL_max
	if newBaseline > c.TTLMax {
		newBaseline = c.TTLMax
	}

	log.Printf("[Cache Evaluation] D_total=%.2f jam | D_avg=%.2f jam | TTL_baseline: %v → %v",
		D_total.Hours(), D_avg.Hours(), c.TTLBaseline, newBaseline)

	// 4. Perbarui TTL_baseline dan reset siklus
	oldBaseline := c.TTLBaseline
	c.TTLBaseline = newBaseline
	c.LastEval = now
	c.NextEval = now.Add(EvalCycleInterval)
	c.CycleStart = now // reset t₀
	c.CleanupCount = 0 // reset counter siklus

	log.Printf("[Cache Evaluation] Siklus baru dimulai. TTL_baseline: %v → %v | t₀ direset",
		oldBaseline, c.TTLBaseline)

	// Bersihkan entry kadaluarsa di akhir siklus
	for k, entry := range c.items {
		if now.After(entry.ExpireAt) {
			delete(c.items, k)
		}
	}
}

// ── Statistik ──────────────────────────────────────────────────────────────────

// GetStats mengembalikan statistik lengkap cache adaptif.
func (c *AdaptiveCache) GetStats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()

	// Hitung rasio hit
	total := c.TotalHits + c.TotalMisses
	hitRatio := 0.0
	if total > 0 {
		hitRatio = float64(c.TotalHits) / float64(total) * 100.0
	}

	// Hitung rata-rata durasi dan TTL aktif dari entry yang masih valid
	var sumDuration, sumActiveTTL float64
	activeCount := 0
	for _, entry := range c.items {
		if now.Before(entry.ExpireAt) {
			sumDuration += time.Since(entry.T0).Seconds()
			sumActiveTTL += time.Until(entry.ExpireAt).Seconds()
			activeCount++
		}
	}

	avgDuration := 0.0
	avgActiveTTL := 0.0
	if activeCount > 0 {
		avgDuration = sumDuration / float64(activeCount)
		avgActiveTTL = sumActiveTTL / float64(activeCount)
	}

	return CacheStats{
		ItemCount:       activeCount,
		TotalHits:       c.TotalHits,
		TotalMisses:     c.TotalMisses,
		HitRatio:        hitRatio,
		AvgDurationSec:  avgDuration,
		AvgActiveTTLSec: avgActiveTTL,
		TTLBaselineSec:  c.TTLBaseline.Seconds(),
		TTLMaxSec:       c.TTLMax.Seconds(),
		AdaptCoeff:      c.AdaptCoeff,
		CleanupCount:    c.CleanupCount,
		CycleStart:      c.CycleStart,
		LastEvalTime:    c.LastEval,
		NextEvalTime:    c.NextEval,
	}
}
