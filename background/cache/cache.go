// Package cache mengimplementasikan mekanisme Auto-Cache Adaptif
// dengan fitur-fitur lanjutan:
//
//  1. JENDELA WAKTU AKTIF (Active Window)
//     Cache hanya aktif pada rentang 06:30 – 24:00.
//     Di luar jendela ini, setiap request langsung ke database (cache dilewati).
//
//  2. TRACKING FREKUENSI PER-KEY
//     Setiap key dicatat jumlah hit dan total durasi aktifnya selama satu siklus.
//     Data ini digunakan sebagai dasar evaluasi bulanan.
//
//  3. EVALUASI SIKLUS BULANAN (30 Hari)
//     Pada akhir setiap siklus 30 hari, sistem mengevaluasi pola akses:
//     - Key FREKUENSI TINGGI  → TTL diperpanjang + masuk daftar pre-cache
//     - Key FREKUENSI RENDAH  → TTL dipersingkat atau tidak di-cache (skip)
//
//  4. ALGORITMA TTL ADAPTIF
//     TTL_runtime = TTL_key + f(D)
//     D           = t_now − entry.T0
//     f(D)        = AdaptCoeff × D
//     TTL dibatasi oleh TTL_max.
//
//  5. KLASIFIKASI KEY
//     HIGH   : hit ≥ HighFreqThreshold → TTL_key = TTL_baseline × HighMultiplier
//     MEDIUM : hit ≥ LowFreqThreshold  → TTL_key = TTL_baseline (tidak berubah)
//     LOW    : hit < LowFreqThreshold  → TTL_key = TTL_baseline × LowMultiplier
//     (atau skip cache jika SkipLowFreq = true)
package cache

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// ── Konstanta ──────────────────────────────────────────────────────────────────

const (
	// TTL dasar saat sistem pertama kali berjalan
	DefaultTTLBaseline = 5 * time.Minute

	// Batas maksimum TTL yang diizinkan
	DefaultTTLMax = 1 * time.Hour

	// Koefisien adaptasi linier:
	// per detik D → TTL bertambah DefaultAdaptCoeff detik
	DefaultAdaptCoeff = 0.001

	// Ambang batas frekuensi hit per siklus untuk klasifikasi
	HighFreqThreshold = 50 // ≥ 50 hit → HIGH
	LowFreqThreshold  = 10 // < 10 hit → LOW

	// Pengali TTL per klasifikasi
	HighFreqMultiplier = 3.0 // HIGH  → TTL × 3
	LowFreqMultiplier  = 0.5 // LOW   → TTL × 0.5

	// Apakah key LOW langsung diskip dari cache?
	SkipLowFreqDefault = false

	// Interval pembersihan entry kadaluarsa
	CleanupInterval = 1 * time.Minute

	// Interval evaluasi siklus bulanan
	EvalCycleInterval = 30 * 24 * time.Hour

	// Jendela aktif cache (HH:MM)
	ActiveWindowStart = "06:30"
	ActiveWindowEnd   = "24:00"
)

// ── Klasifikasi Frekuensi ──────────────────────────────────────────────────────

// FreqClass merepresentasikan klasifikasi frekuensi akses sebuah key.
type FreqClass string

const (
	FreqHigh   FreqClass = "HIGH"
	FreqMedium FreqClass = "MEDIUM"
	FreqLow    FreqClass = "LOW"
)

// ── Statistik Per-Key ──────────────────────────────────────────────────────────

// KeyStat menyimpan statistik akses satu key selama satu siklus evaluasi.
type KeyStat struct {
	Key           string        `json:"key"`
	HitCount      int64         `json:"hit_count"`
	MissCount     int64         `json:"miss_count"`
	TotalDuration time.Duration `json:"total_active_duration_ns"`
	Class         FreqClass     `json:"class"`
	AssignedTTL   time.Duration `json:"assigned_ttl_ns"`
	SkipCache     bool          `json:"skip_cache"`
	LastAccess    time.Time     `json:"last_access"`
}

// ── CacheEntry ─────────────────────────────────────────────────────────────────

// CacheEntry menyimpan satu item beserta metadata temporalnya.
type CacheEntry struct {
	Value    interface{}
	ExpireAt time.Time // t_expire = t_set + TTL_runtime
	T0       time.Time // waktu entry pertama kali disimpan ke cache
	HitCount int64     // jumlah hit pada entry ini
}

// ── CacheStats ─────────────────────────────────────────────────────────────────

// CacheStats berisi ringkasan lengkap kondisi cache untuk satu siklus.
type CacheStats struct {
	// Kondisi saat ini
	ItemCount       int     `json:"item_count"`
	TotalHits       int64   `json:"total_hits"`
	TotalMisses     int64   `json:"total_misses"`
	HitRatioPct     float64 `json:"hit_ratio_pct"`
	AvgDurationSec  float64 `json:"avg_entry_duration_seconds"`
	AvgActiveTTLSec float64 `json:"avg_active_ttl_seconds"`

	// Parameter adaptif
	TTLBaselineSec float64 `json:"ttl_baseline_seconds"`
	TTLMaxSec      float64 `json:"ttl_max_seconds"`
	AdaptCoeff     float64 `json:"adapt_coeff"`

	// Jendela aktif
	ActiveWindowStart string `json:"active_window_start"`
	ActiveWindowEnd   string `json:"active_window_end"`
	IsWindowActive    bool   `json:"is_window_active_now"`
	WindowStatus      string `json:"window_status"`

	// Siklus evaluasi
	CleanupCount int64     `json:"entries_cleaned_this_cycle"`
	CycleStart   time.Time `json:"cycle_start_t0"`
	LastEvalTime time.Time `json:"last_eval_time"`
	NextEvalTime time.Time `json:"next_eval_time"`

	// Statistik per-key hasil evaluasi
	KeyStats     []KeyStat `json:"key_stats"`
	HighFreqKeys []string  `json:"high_freq_keys"`
	LowFreqKeys  []string  `json:"low_freq_keys"`
	SkippedKeys  []string  `json:"skipped_keys"`
}

// ── AdaptiveCache ──────────────────────────────────────────────────────────────

// AdaptiveCache adalah in-memory cache dengan TTL dinamis berbasis durasi
// dan evaluasi frekuensi per siklus bulanan.
type AdaptiveCache struct {
	mu    sync.RWMutex
	items map[string]*CacheEntry

	// Parameter adaptasi global
	TTLBaseline time.Duration
	TTLMax      time.Duration
	AdaptCoeff  float64
	SkipLowFreq bool

	// TTL per-key hasil evaluasi bulanan (override TTLBaseline)
	keyTTL  map[string]time.Duration // TTL yang ditetapkan per key
	skipKey map[string]bool          // key yang diskip dari cache

	// Statistik per-key dalam siklus berjalan
	keyStats map[string]*KeyStat

	// Metadata siklus
	CycleStart time.Time
	LastEval   time.Time
	NextEval   time.Time

	// Statistik global
	TotalHits    int64
	TotalMisses  int64
	CleanupCount int64
}

// ── Konstruktor ────────────────────────────────────────────────────────────────

// NewAdaptiveCache membuat instance cache adaptif dengan konfigurasi default.
func NewAdaptiveCache() *AdaptiveCache {
	now := time.Now()
	c := &AdaptiveCache{
		items:       make(map[string]*CacheEntry),
		keyTTL:      make(map[string]time.Duration),
		skipKey:     make(map[string]bool),
		keyStats:    make(map[string]*KeyStat),
		TTLBaseline: DefaultTTLBaseline,
		TTLMax:      DefaultTTLMax,
		AdaptCoeff:  DefaultAdaptCoeff,
		SkipLowFreq: SkipLowFreqDefault,
		CycleStart:  now,
		LastEval:    now,
		NextEval:    now.Add(EvalCycleInterval),
	}
	log.Printf("[AdaptiveCache] Init → TTL_baseline=%v TTL_max=%v AdaptCoeff=%.4f window=%s–%s",
		c.TTLBaseline, c.TTLMax, c.AdaptCoeff, ActiveWindowStart, ActiveWindowEnd)
	return c
}

// ── Jendela Waktu Aktif ────────────────────────────────────────────────────────

// IsWindowActive mengecek apakah waktu sekarang berada dalam jendela aktif
// cache (06:30 – 24:00).
//
//	06:30 = menit ke-390
//	24:00 = menit ke-1440 (setara batas akhir hari, 23:59 termasuk aktif)
func IsWindowActive() bool {
	now := time.Now()
	h, m, _ := now.Clock()
	totalMinutes := h*60 + m

	startMinutes := 6*60 + 30 // 390
	endMinutes := 24 * 60     // 1440

	return totalMinutes >= startMinutes && totalMinutes < endMinutes
}

// windowStatusString mengembalikan deskripsi status jendela aktif.
func windowStatusString() string {
	if IsWindowActive() {
		return fmt.Sprintf("AKTIF (window %s–%s)", ActiveWindowStart, ActiveWindowEnd)
	}
	h, m, _ := time.Now().Clock()
	return fmt.Sprintf("NONAKTIF — sekarang %02d:%02d, di luar window %s–%s",
		h, m, ActiveWindowStart, ActiveWindowEnd)
}

// ── Perhitungan TTL Per-Key ────────────────────────────────────────────────────

// baselineTTLForKey mengembalikan TTL dasar yang berlaku untuk key tertentu.
// Menggunakan override hasil evaluasi bulanan jika ada, atau TTLBaseline global.
func (c *AdaptiveCache) baselineTTLForKey(key string) time.Duration {
	if ttl, ok := c.keyTTL[key]; ok {
		return ttl
	}
	return c.TTLBaseline
}

// computeRuntimeTTL menghitung TTL_runtime:
//
//	TTL_runtime = TTL_key + f(D)
//	f(D)        = AdaptCoeff × D.Seconds()
//
// Dibatasi oleh TTL_max.
func (c *AdaptiveCache) computeRuntimeTTL(key string, entry *CacheEntry) time.Duration {
	D := time.Since(entry.T0)
	keyBaseline := c.baselineTTLForKey(key)

	adaptedSec := keyBaseline.Seconds() + c.AdaptCoeff*D.Seconds()
	runtimeTTL := time.Duration(adaptedSec * float64(time.Second))

	if runtimeTTL > c.TTLMax {
		runtimeTTL = c.TTLMax
	}
	return runtimeTTL
}

// ── Pencatatan Statistik Per-Key ──────────────────────────────────────────────

func (c *AdaptiveCache) recordHit(key string, entry *CacheEntry) {
	stat := c.ensureKeyStat(key)
	stat.HitCount++
	stat.TotalDuration += time.Since(entry.T0)
	stat.LastAccess = time.Now()
}

func (c *AdaptiveCache) recordMiss(key string) {
	stat := c.ensureKeyStat(key)
	stat.MissCount++
	stat.LastAccess = time.Now()
}

func (c *AdaptiveCache) ensureKeyStat(key string) *KeyStat {
	if _, ok := c.keyStats[key]; !ok {
		c.keyStats[key] = &KeyStat{
			Key:         key,
			AssignedTTL: c.TTLBaseline,
			Class:       FreqMedium,
		}
	}
	return c.keyStats[key]
}

// ── Operasi Cache Utama ────────────────────────────────────────────────────────

// Get mengambil item dari cache.
//
//   - Di luar jendela aktif (< 06:30) → lewati cache, langsung ke DB
//   - Key masuk skip list (LOW freq)  → lewati cache
//   - Entry kadaluarsa               → hapus, return miss
//   - HIT                            → perbarui TTL adaptif, catat statistik
func (c *AdaptiveCache) Get(key string) (interface{}, bool) {
	// ── Cek jendela waktu ──
	if !IsWindowActive() {
		log.Printf("[Cache WINDOW-SKIP] key=%q — %s", key, windowStatusString())
		c.mu.Lock()
		c.TotalMisses++
		c.recordMiss(key)
		c.mu.Unlock()
		return nil, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// ── Cek skip list (hasil evaluasi LOW freq) ──
	if c.skipKey[key] {
		log.Printf("[Cache FREQ-SKIP] key=%q — klasifikasi LOW, langsung ke DB", key)
		c.TotalMisses++
		c.recordMiss(key)
		return nil, false
	}

	entry, exists := c.items[key]
	if !exists {
		c.TotalMisses++
		c.recordMiss(key)
		return nil, false
	}

	// ── Cek kadaluarsa ──
	if time.Now().After(entry.ExpireAt) {
		delete(c.items, key)
		c.CleanupCount++
		c.TotalMisses++
		c.recordMiss(key)
		return nil, false
	}

	// ── Cache HIT ──
	c.TotalHits++
	entry.HitCount++
	c.recordHit(key, entry)

	newTTL := c.computeRuntimeTTL(key, entry)
	entry.ExpireAt = time.Now().Add(newTTL)

	class := c.keyStats[key].Class
	log.Printf("[Cache HIT] key=%q class=%s D=%.1fs TTL_runtime=%v expire=%v",
		key, class,
		time.Since(entry.T0).Seconds(),
		newTTL.Round(time.Second),
		entry.ExpireAt.Format("15:04:05"),
	)

	return entry.Value, true
}

// Set menyimpan item ke cache.
//
//   - Di luar jendela aktif → tidak disimpan
//   - Key dalam skip list   → tidak disimpan
//   - Key sudah ada         → perbarui value + hitung ulang TTL
//   - Key baru              → buat entry baru dengan TTL_key
func (c *AdaptiveCache) Set(key string, value interface{}) {
	// ── Cek jendela waktu ──
	if !IsWindowActive() {
		log.Printf("[Cache SET-WINDOW-SKIP] key=%q — %s, tidak disimpan", key, windowStatusString())
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// ── Cek skip list ──
	if c.skipKey[key] {
		log.Printf("[Cache SET-FREQ-SKIP] key=%q — LOW freq, tidak disimpan", key)
		return
	}

	now := time.Now()

	// Update entry yang sudah ada
	if existing, ok := c.items[key]; ok {
		existing.Value = value
		newTTL := c.computeRuntimeTTL(key, existing)
		existing.ExpireAt = now.Add(newTTL)
		log.Printf("[Cache SET-UPDATE] key=%q TTL_runtime=%v", key, newTTL.Round(time.Second))
		return
	}

	// Entry baru
	keyBaseline := c.baselineTTLForKey(key)
	entry := &CacheEntry{
		Value:    value,
		T0:       now,
		ExpireAt: now.Add(keyBaseline),
	}
	c.items[key] = entry
	c.ensureKeyStat(key)

	log.Printf("[Cache SET-NEW] key=%q TTL_key=%v expire=%v",
		key, keyBaseline.Round(time.Second), entry.ExpireAt.Format("15:04:05"))
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

// DeleteByPrefix menghapus semua item dengan awalan prefix tertentu.
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

// AutoCleanup menjalankan pembersihan entry kadaluarsa setiap CleanupInterval.
func (c *AdaptiveCache) AutoCleanup() {
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()
	log.Println("[Cache] AutoCleanup goroutine dimulai, interval:", CleanupInterval)
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
		if now.After(entry.ExpireAt) {
			delete(c.items, k)
			c.CleanupCount++
			removed++
		}
	}
	if removed > 0 {
		log.Printf("[Cache Cleanup] %d entry dihapus (total siklus: %d)", removed, c.CleanupCount)
	}
}

// EvaluationCycle menjalankan evaluasi bulanan setiap EvalCycleInterval.
func (c *AdaptiveCache) EvaluationCycle() {
	ticker := time.NewTicker(EvalCycleInterval)
	defer ticker.Stop()
	log.Println("[Cache] EvaluationCycle goroutine dimulai, interval:", EvalCycleInterval)
	for range ticker.C {
		c.runEvaluation()
	}
}

// ── Evaluasi Siklus Bulanan ────────────────────────────────────────────────────

// runEvaluation menjalankan satu siklus evaluasi penuh:
//
//  1. Re-kalibrasi TTL_baseline global berdasarkan D_total dan D_avg
//  2. Klasifikasi setiap key: HIGH / MEDIUM / LOW
//  3. Tetapkan TTL_key baru berdasarkan kelas
//  4. Tandai key LOW ke skipKey jika SkipLowFreq aktif
//  5. Bersihkan entry kadaluarsa
//  6. Reset semua counter siklus
func (c *AdaptiveCache) runEvaluation() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	// ── 1. Re-kalibrasi TTL_baseline global ──────────────────────────────
	D_total := now.Sub(c.CycleStart)
	D_avg := D_total / 30
	g_Davg := time.Duration(c.AdaptCoeff * float64(D_avg))
	newBaseline := c.TTLBaseline + g_Davg
	if newBaseline > c.TTLMax {
		newBaseline = c.TTLMax
	}
	log.Printf("[Evaluation] D_total=%.2fh D_avg=%.2fh TTL_baseline: %v → %v",
		D_total.Hours(), D_avg.Hours(), c.TTLBaseline, newBaseline)
	c.TTLBaseline = newBaseline

	// ── 2–4. Klasifikasi dan penetapan TTL per-key ────────────────────────
	highKeys, lowKeys, skipKeys := []string{}, []string{}, []string{}

	for key, stat := range c.keyStats {
		var class FreqClass
		var assignedTTL time.Duration
		var skip bool

		switch {
		case stat.HitCount >= HighFreqThreshold:
			// TINGGI: prioritas pre-cache, TTL diperpanjang
			class = FreqHigh
			assignedTTL = time.Duration(float64(c.TTLBaseline) * HighFreqMultiplier)
			if assignedTTL > c.TTLMax {
				assignedTTL = c.TTLMax
			}
			highKeys = append(highKeys, key)

		case stat.HitCount >= LowFreqThreshold:
			// SEDANG: TTL tidak berubah dari baseline
			class = FreqMedium
			assignedTTL = c.TTLBaseline

		default:
			// RENDAH: TTL dipersingkat, opsional skip
			class = FreqLow
			assignedTTL = time.Duration(float64(c.TTLBaseline) * LowFreqMultiplier)
			lowKeys = append(lowKeys, key)
			if c.SkipLowFreq {
				skip = true
				skipKeys = append(skipKeys, key)
			}
		}

		stat.Class = class
		stat.AssignedTTL = assignedTTL
		stat.SkipCache = skip
		c.keyTTL[key] = assignedTTL
		c.skipKey[key] = skip

		log.Printf("[Evaluation] key=%-32q hits=%-5d class=%-6s TTL=%v skip=%v",
			key, stat.HitCount, class, assignedTTL.Round(time.Second), skip)
	}

	// ── 5. Pre-cache hint (log untuk monitoring) ──────────────────────────
	if len(highKeys) > 0 {
		log.Printf("[Evaluation] PRE-CACHE priority (%d key): %v", len(highKeys), highKeys)
	}
	if len(skipKeys) > 0 {
		log.Printf("[Evaluation] SKIP-CACHE (%d key): %v", len(skipKeys), skipKeys)
	}

	// ── 6. Bersihkan entry kadaluarsa ─────────────────────────────────────
	for k, entry := range c.items {
		if now.After(entry.ExpireAt) {
			delete(c.items, k)
		}
	}

	// ── 7. Reset metadata siklus ──────────────────────────────────────────
	c.LastEval = now
	c.NextEval = now.Add(EvalCycleInterval)
	c.CycleStart = now
	c.CleanupCount = 0

	// Reset counter hit/miss per-key (kelas dan TTL dipertahankan)
	for _, stat := range c.keyStats {
		stat.HitCount = 0
		stat.MissCount = 0
		stat.TotalDuration = 0
	}

	log.Printf("[Evaluation] Siklus baru dimulai. HIGH=%d MEDIUM=%d LOW=%d SKIP=%d",
		len(highKeys),
		len(c.keyStats)-len(highKeys)-len(lowKeys),
		len(lowKeys),
		len(skipKeys),
	)
}

// ── Statistik ──────────────────────────────────────────────────────────────────

// GetStats mengembalikan statistik lengkap cache adaptif.
func (c *AdaptiveCache) GetStats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()

	total := c.TotalHits + c.TotalMisses
	hitRatio := 0.0
	if total > 0 {
		hitRatio = float64(c.TotalHits) / float64(total) * 100.0
	}

	var sumDur, sumTTL float64
	activeCount := 0
	for _, entry := range c.items {
		if now.Before(entry.ExpireAt) {
			sumDur += time.Since(entry.T0).Seconds()
			sumTTL += time.Until(entry.ExpireAt).Seconds()
			activeCount++
		}
	}
	avgDur, avgTTL := 0.0, 0.0
	if activeCount > 0 {
		avgDur = sumDur / float64(activeCount)
		avgTTL = sumTTL / float64(activeCount)
	}

	keyStatSlice := make([]KeyStat, 0, len(c.keyStats))
	highKeys, lowKeys, skippedKeys := []string{}, []string{}, []string{}
	for _, stat := range c.keyStats {
		keyStatSlice = append(keyStatSlice, *stat)
		switch stat.Class {
		case FreqHigh:
			highKeys = append(highKeys, stat.Key)
		case FreqLow:
			lowKeys = append(lowKeys, stat.Key)
		}
		if stat.SkipCache {
			skippedKeys = append(skippedKeys, stat.Key)
		}
	}

	return CacheStats{
		ItemCount:       activeCount,
		TotalHits:       c.TotalHits,
		TotalMisses:     c.TotalMisses,
		HitRatioPct:     hitRatio,
		AvgDurationSec:  avgDur,
		AvgActiveTTLSec: avgTTL,

		TTLBaselineSec: c.TTLBaseline.Seconds(),
		TTLMaxSec:      c.TTLMax.Seconds(),
		AdaptCoeff:     c.AdaptCoeff,

		ActiveWindowStart: ActiveWindowStart,
		ActiveWindowEnd:   ActiveWindowEnd,
		IsWindowActive:    IsWindowActive(),
		WindowStatus:      windowStatusString(),

		CleanupCount: c.CleanupCount,
		CycleStart:   c.CycleStart,
		LastEvalTime: c.LastEval,
		NextEvalTime: c.NextEval,

		KeyStats:     keyStatSlice,
		HighFreqKeys: highKeys,
		LowFreqKeys:  lowKeys,
		SkippedKeys:  skippedKeys,
	}
}

// ── Konfigurasi Runtime ────────────────────────────────────────────────────────

// SetSkipLowFreq mengubah perilaku key LOW freq secara runtime.
// true  = key LOW langsung ke DB (skip cache)
// false = key LOW tetap di-cache dengan TTL lebih pendek
func (c *AdaptiveCache) SetSkipLowFreq(skip bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SkipLowFreq = skip
	log.Printf("[Cache Config] SkipLowFreq → %v", skip)
}

// ForceEvaluate memaksa evaluasi siklus langsung tanpa menunggu 30 hari.
// Berguna untuk keperluan testing.
func (c *AdaptiveCache) ForceEvaluate() {
	log.Println("[Cache] ForceEvaluate dipanggil manual.")
	c.runEvaluation()
}
