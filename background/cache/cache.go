// Package cache mengimplementasikan Auto-Cache Adaptif berbasis tiga pilar:
//
// ══════════════════════════════════════════════════════════════════════════════
//
//	TIGA PILAR ALGORITMA
//
// ══════════════════════════════════════════════════════════════════════════════
//
//	PILAR 1 — AKSES AWAL (First Access Time)
//	  Setiap hari, system mencatat JAM PERTAMA kali setiap key diakses.
//	  Contoh: key "products:all" pertama kali diakses pukul 06:30 setiap hari.
//	  → Pada bulan berikutnya, cache di-PRE-WARM otomatis pada jam yang sama
//	    sebelum pengguna datang, sehingga request pertama sudah HIT.
//	  → AvgFirstAccessHour & AvgFirstAccessMinute dihitung dari rata-rata
//	    jam akses pertama harian selama sebulan.
//
//	PILAR 2 — CACHE HIT (Data Populer)
//	  Hanya key yang BENAR-BENAR diakses (HitCount > 0) yang masuk daftar
//	  "hot key" — kandidat autocache bulan depan.
//	  → Key dengan hit lebih banyak = prioritas lebih tinggi.
//	  → Key yang tidak pernah di-hit tidak di-cache bulan depan.
//
//	PILAR 3 — DURASI CACHE (Active Duration)
//	  Durasi = LastAccessTime − FirstAccessTime dalam sebulan.
//	  Ini mencerminkan "seberapa lama data ini aktif digunakan."
//	  → Durasi ini menjadi TTL key tersebut di bulan berikutnya.
//	  → Semakin lama data dipakai, semakin lama pula ia di-cache bulan depan.
//
// ══════════════════════════════════════════════════════════════════════════════
//
//	ALUR LENGKAP
//
// ══════════════════════════════════════════════════════════════════════════════
//
//	[Bulan Berjalan]
//	  Setiap cache HIT pada key K:
//	    1. Catat jam akses pertama hari ini → DailyFirstAccess[tanggal]
//	    2. Perbarui LastAccessTime
//	    3. Tambah HitCount
//	    4. Hitung TTL_runtime = TTL_baseline + AdaptCoeff × D_key
//	       (D_key = LastAccess − FirstAccess key ini bulan ini)
//
//	[Evaluasi Pergantian Bulan]
//	  Untuk setiap key yang pernah di-hit:
//	    AvgFirstHour   = rata-rata jam pertama akses harian
//	    AvgFirstMinute = rata-rata menit pertama akses harian
//	    SuggestedTTL   = LastAccessTime − FirstAccessTime (durasi aktif bulan ini)
//	    → Simpan sebagai HotKeyProfile
//
//	[Bulan Berikutnya]
//	  WarmupScheduler berjalan setiap menit:
//	    Jika jam sekarang = AvgFirstHour:AvgFirstMinute suatu hot key
//	      → Panggil WarmupFunc(key) → ambil data dari DB → simpan ke cache
//	      → Cache sudah siap SEBELUM pengguna datang
//
// ══════════════════════════════════════════════════════════════════════════════
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
	DefaultTTLBaseline  = 5 * time.Minute
	DefaultTTLMax       = 4 * time.Hour
	DefaultAdaptCoeff   = 0.002
	CleanupInterval     = 1 * time.Minute
	WarmupCheckInterval = 1 * time.Minute // interval cek jadwal pre-warm
)

// ── Tipe Callback ──────────────────────────────────────────────────────────────

// WarmupFunc adalah fungsi yang dipanggil saat pre-warm cache.
// Implementasinya ada di handler — mengambil data dari DB berdasarkan key.
type WarmupFunc func(key string) (interface{}, error)

// ── Struktur Data Per-Key ──────────────────────────────────────────────────────

// dailyRecord mencatat aktivitas satu key pada satu hari tertentu.
type dailyRecord struct {
	Date      string    // format: "2006-01-02"
	FirstTime time.Time // jam pertama kali key ini diakses hari itu (Pilar 1)
	LastTime  time.Time // jam terakhir key ini diakses hari itu
	Hits      int64     // jumlah hit hari ini
}

// keyMonthlyStats mengumpulkan statistik satu key selama satu bulan penuh.
type keyMonthlyStats struct {
	Key             string
	Month           int
	Year            int
	FirstAccessEver time.Time               // akses pertama key ini bulan ini
	LastAccessEver  time.Time               // akses terakhir key ini bulan ini
	TotalHits       int64                   // total hit bulan ini (Pilar 2)
	DailyRecords    map[string]*dailyRecord // key: "2006-01-02" (Pilar 1 per hari)
}

// HotKeyProfile adalah hasil evaluasi bulanan untuk satu key.
// Profil ini dipakai bulan depan untuk menentukan kapan & berapa lama cache.
type HotKeyProfile struct {
	Key               string        `json:"key"`
	HitCountLastMonth int64         `json:"hit_count_last_month"`    // Pilar 2
	AvgFirstHour      int           `json:"avg_first_access_hour"`   // Pilar 1
	AvgFirstMinute    int           `json:"avg_first_access_minute"` // Pilar 1
	SuggestedTTL      time.Duration `json:"suggested_ttl"`           // Pilar 3
	ActiveDurationSec float64       `json:"active_duration_seconds"` // Pilar 3
	EvalMonth         string        `json:"eval_month"`
	warmedUpToday     bool          // flag internal: sudah di-warm hari ini?
	lastWarmDate      string        // tanggal terakhir di-warm
}

// ── Struktur Cache Entry ───────────────────────────────────────────────────────

// CacheEntry menyimpan data beserta metadata tracking per-key.
type CacheEntry struct {
	Value    interface{}
	ExpireAt time.Time
	stats    *keyMonthlyStats // pointer ke stats bulan ini
}

// ── Statistik & Snapshot ───────────────────────────────────────────────────────

// MonthlyEvalSnapshot menyimpan ringkasan evaluasi satu bulan.
type MonthlyEvalSnapshot struct {
	Month        string          `json:"month"`
	HotKeysFound int             `json:"hot_keys_found"`
	Profiles     []HotKeyProfile `json:"hot_key_profiles"`
	EvaluatedAt  time.Time       `json:"evaluated_at"`
}

// CacheStats berisi statistik lengkap sistem.
type CacheStats struct {
	ItemCount      int                   `json:"item_count"`
	TotalHits      int64                 `json:"total_hits"`
	TotalMisses    int64                 `json:"total_misses"`
	HitRatioPct    float64               `json:"hit_ratio_pct"`
	CurrentMonth   string                `json:"current_month"`
	TTLBaselineSec float64               `json:"ttl_baseline_seconds"`
	TTLMaxSec      float64               `json:"ttl_max_seconds"`
	AdaptCoeff     float64               `json:"adapt_coeff"`
	CleanupCount   int64                 `json:"entries_cleaned_total"`
	HotKeyProfiles []HotKeyProfile       `json:"hot_key_profiles_next_month"`
	MonthlyHistory []MonthlyEvalSnapshot `json:"monthly_eval_history"`
	ActiveKeyStats []ActiveKeyStat       `json:"active_key_stats"`
}

// ActiveKeyStat adalah ringkasan statistik key yang saat ini aktif di cache.
type ActiveKeyStat struct {
	Key               string  `json:"key"`
	HitCount          int64   `json:"hit_count_this_month"`
	ActiveDurationSec float64 `json:"active_duration_seconds"`
	AvgFirstHour      int     `json:"avg_first_access_hour"`
	AvgFirstMinute    int     `json:"avg_first_access_minute"`
	TTLRemainingSec   float64 `json:"ttl_remaining_seconds"`
}

// ── AdaptiveCache ──────────────────────────────────────────────────────────────

// AdaptiveCache adalah in-memory cache dengan tiga pilar adaptasi.
type AdaptiveCache struct {
	mu    sync.RWMutex
	items map[string]*CacheEntry

	// Parameter adaptasi
	TTLBaseline time.Duration
	TTLMax      time.Duration
	AdaptCoeff  float64

	// Tracking bulan berjalan
	currentMonth int
	currentYear  int

	// Hot key profiles untuk bulan ini (hasil eval bulan lalu)
	hotKeyProfiles map[string]*HotKeyProfile

	// Riwayat evaluasi bulanan
	evalHistory []MonthlyEvalSnapshot

	// Warmup callback (diisi dari luar oleh handler)
	warmupFunc WarmupFunc

	// Statistik global
	TotalHits    int64
	TotalMisses  int64
	CleanupCount int64
}

// ── Konstruktor ────────────────────────────────────────────────────────────────

// NewAdaptiveCache membuat instance cache baru.
func NewAdaptiveCache() *AdaptiveCache {
	now := time.Now()
	c := &AdaptiveCache{
		items:          make(map[string]*CacheEntry),
		hotKeyProfiles: make(map[string]*HotKeyProfile),
		evalHistory:    []MonthlyEvalSnapshot{},
		TTLBaseline:    DefaultTTLBaseline,
		TTLMax:         DefaultTTLMax,
		AdaptCoeff:     DefaultAdaptCoeff,
		currentMonth:   int(now.Month()),
		currentYear:    now.Year(),
	}
	log.Printf("[Cache] ══ Auto-Cache Adaptif Aktif ══")
	log.Printf("[Cache] TTL_baseline  = %v", c.TTLBaseline)
	log.Printf("[Cache] TTL_max       = %v", c.TTLMax)
	log.Printf("[Cache] AdaptCoeff    = %.4f", c.AdaptCoeff)
	log.Printf("[Cache] Bulan aktif   = %s", monthLabel(now.Year(), int(now.Month())))
	log.Printf("[Cache] Tiga pilar: (1) Jam akses awal  (2) Cache hit  (3) Durasi aktif")
	return c
}

// RegisterWarmupFunc mendaftarkan fungsi yang akan dipanggil saat pre-warm.
// Harus dipanggil dari main.go setelah handler siap.
func (c *AdaptiveCache) RegisterWarmupFunc(fn WarmupFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.warmupFunc = fn
	log.Println("[Cache] WarmupFunc terdaftar — pre-warming otomatis aktif.")
}

// ── Perhitungan TTL Per-Key ────────────────────────────────────────────────────

// computeKeyTTL menghitung TTL untuk key berdasarkan durasi aktifnya bulan ini.
//
//   D_key       = LastAccessEver − FirstAccessEver  (durasi aktif key bulan ini)
//   TTL_runtime = TTL_baseline + AdaptCoeff × D_key.Seconds()  ≤ TTL_max
//
// Jika key punya profil dari bulan lalu (hot key), gunakan SuggestedTTL sebagai baseline.
func (c *AdaptiveCache) computeKeyTTL(key string, stats *keyMonthlyStats) time.Duration {
	baseline := c.TTLBaseline

	// Jika key ini adalah hot key dari bulan lalu, gunakan SuggestedTTL-nya
	if profile, isHot := c.hotKeyProfiles[key]; isHot {
		if profile.SuggestedTTL > baseline {
			baseline = profile.SuggestedTTL
			log.Printf("[Cache TTL] key=%q menggunakan SuggestedTTL dari profil bulan lalu: %v",
				key, baseline.Round(time.Second))
		}
	}

	// Pilar 3: hitung durasi aktif key bulan ini
	var D_key time.Duration
	if !stats.FirstAccessEver.IsZero() && !stats.LastAccessEver.IsZero() {
		D_key = stats.LastAccessEver.Sub(stats.FirstAccessEver)
	}

	adaptedSec := baseline.Seconds() + c.AdaptCoeff*D_key.Seconds()
	runtime := time.Duration(adaptedSec * float64(time.Second))

	if runtime > c.TTLMax {
		runtime = c.TTLMax
	}
	return runtime
}

// ── Operasi Cache ──────────────────────────────────────────────────────────────

// Get mengambil item dari cache.
//
// Saat HIT — tiga pilar dicatat:
//   Pilar 1: catat jam akses pertama hari ini
//   Pilar 2: tambah HitCount
//   Pilar 3: perbarui LastAccessTime → perpanjang durasi aktif
func (c *AdaptiveCache) Get(key string) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, exists := c.items[key]
	if !exists {
		c.TotalMisses++
		return nil, false
	}

	now := time.Now()

	// Cek kadaluarsa
	if now.After(entry.ExpireAt) {
		delete(c.items, key)
		c.CleanupCount++
		c.TotalMisses++
		return nil, false
	}

	// ── Cache HIT ──
	c.TotalHits++
	stats := entry.stats

	// PILAR 2: Tambah HitCount
	stats.TotalHits++

	// PILAR 1: Catat jam akses pertama hari ini
	today := now.Format("2006-01-02")
	if _, exists := stats.DailyRecords[today]; !exists {
		stats.DailyRecords[today] = &dailyRecord{
			Date:      today,
			FirstTime: now, // ← JAM PERTAMA akses hari ini (Pilar 1)
			LastTime:  now,
			Hits:      1,
		}
		log.Printf("[Cache HIT][Pilar 1] key=%-25q | jam akses pertama hari ini: %s",
			key, now.Format("15:04:05"))
	} else {
		stats.DailyRecords[today].LastTime = now
		stats.DailyRecords[today].Hits++
	}

	// PILAR 3: Perbarui LastAccessEver → perpanjang durasi aktif
	stats.LastAccessEver = now

	// Hitung ulang TTL berdasarkan durasi aktif key ini
	newTTL := c.computeKeyTTL(key, stats)
	entry.ExpireAt = now.Add(newTTL)

	D_key := stats.LastAccessEver.Sub(stats.FirstAccessEver)
	log.Printf("[Cache HIT]  key=%-25q | hits=%d | D_key=%6.1fs | TTL_runtime=%-10v | expire=%s",
		key,
		stats.TotalHits,
		D_key.Seconds(),
		newTTL.Round(time.Second),
		entry.ExpireAt.Format("15:04:05"),
	)

	return entry.Value, true
}

// Set menyimpan item ke cache dan menginisialisasi stats tracking.
func (c *AdaptiveCache) Set(key string, value interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	today := now.Format("2006-01-02")

	// Entry sudah ada → update value saja, pertahankan stats
	if existing, ok := c.items[key]; ok {
		existing.Value = value
		newTTL := c.computeKeyTTL(key, existing.stats)
		existing.ExpireAt = now.Add(newTTL)
		log.Printf("[Cache SET-UPDATE] key=%-25q | TTL=%v", key, newTTL.Round(time.Second))
		return
	}

	// Entry baru → buat stats baru, gunakan TTL dari profil hot key jika ada
	stats := &keyMonthlyStats{
		Key:             key,
		Month:           c.currentMonth,
		Year:            c.currentYear,
		FirstAccessEver: now,
		LastAccessEver:  now,
		TotalHits:       0,
		DailyRecords: map[string]*dailyRecord{
			today: {
				Date:      today,
				FirstTime: now,
				LastTime:  now,
				Hits:      0,
			},
		},
	}

	// Tentukan TTL awal
	ttl := c.TTLBaseline
	if profile, isHot := c.hotKeyProfiles[key]; isHot {
		if profile.SuggestedTTL > ttl {
			ttl = profile.SuggestedTTL
		}
	}

	c.items[key] = &CacheEntry{
		Value:    value,
		ExpireAt: now.Add(ttl),
		stats:    stats,
	}

	log.Printf("[Cache SET-NEW]    key=%-25q | TTL_init=%v | expire=%s",
		key, ttl.Round(time.Second), now.Add(ttl).Format("15:04:05"))
}

// Delete menghapus satu item (invalidasi CRUD).
func (c *AdaptiveCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; ok {
		delete(c.items, key)
		log.Printf("[Cache INVALIDATE] key=%q dihapus (CRUD)", key)
	}
}

// DeleteByPrefix menghapus semua item dengan prefix tertentu.
func (c *AdaptiveCache) DeleteByPrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k := range c.items {
		if strings.HasPrefix(k, prefix) {
			delete(c.items, k)
			n++
		}
	}
	if n > 0 {
		log.Printf("[Cache INVALIDATE-PREFIX] prefix=%q → %d item dihapus", prefix, n)
	}
}

// ── Goroutine Background ───────────────────────────────────────────────────────

// AutoCleanup menjalankan pembersihan entry kadaluarsa setiap menit.
func (c *AdaptiveCache) AutoCleanup() {
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()
	log.Println("[Cache] AutoCleanup goroutine aktif.")
	for range ticker.C {
		c.runCleanup()
	}
}

func (c *AdaptiveCache) runCleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	n := 0
	for k, entry := range c.items {
		if now.After(entry.ExpireAt) {
			delete(c.items, k)
			c.CleanupCount++
			n++
		}
	}
	if n > 0 {
		log.Printf("[Cache Cleanup] %d entry kadaluarsa dihapus.", n)
	}
}

// MonthlyEvaluationCycle memeriksa pergantian bulan setiap jam.
func (c *AdaptiveCache) MonthlyEvaluationCycle() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	log.Println("[Cache] MonthlyEvaluationCycle goroutine aktif (cek tiap 1 jam).")
	for range ticker.C {
		now := time.Now()
		c.mu.RLock()
		changed := int(now.Month()) != c.currentMonth || now.Year() != c.currentYear
		c.mu.RUnlock()
		if changed {
			c.runMonthlyEvaluation(now)
		}
	}
}

// runMonthlyEvaluation menjalankan evaluasi tiga pilar saat bulan berganti.
//
//  Untuk setiap key yang pernah di-hit bulan ini:
//    Pilar 1: hitung AvgFirstHour & AvgFirstMinute dari DailyRecords
//    Pilar 2: simpan HitCount (hanya key dengan hit > 0 yang jadi hot key)
//    Pilar 3: SuggestedTTL = LastAccessEver − FirstAccessEver (durasi aktif)
func (c *AdaptiveCache) runMonthlyEvaluation(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	prevMonth := c.currentMonth
	prevYear := c.currentYear

	log.Printf("[Cache Eval] ══ EVALUASI BULANAN: %s → %s ══",
		monthLabel(prevYear, prevMonth),
		monthLabel(now.Year(), int(now.Month())),
	)

	// Kumpulkan semua stats dari entry aktif
	allStats := []*keyMonthlyStats{}
	for _, entry := range c.items {
		if entry.stats != nil && entry.stats.Month == prevMonth && entry.stats.Year == prevYear {
			allStats = append(allStats, entry.stats)
		}
	}

	// Bangun hot key profiles baru
	newProfiles := make(map[string]*HotKeyProfile)
	snapshots := []HotKeyProfile{}

	for _, stats := range allStats {
		// PILAR 2: Hanya key yang pernah di-hit
		if stats.TotalHits == 0 {
			log.Printf("[Cache Eval] key=%q dilewati (tidak ada hit bulan ini)", stats.Key)
			continue
		}

		// PILAR 1: Hitung rata-rata jam & menit akses pertama harian
		avgHour, avgMinute := computeAvgFirstAccess(stats.DailyRecords)

		// PILAR 3: SuggestedTTL = durasi aktif (LastAccess − FirstAccess)
		activeDuration := stats.LastAccessEver.Sub(stats.FirstAccessEver)
		if activeDuration < c.TTLBaseline {
			activeDuration = c.TTLBaseline // minimal TTL_baseline
		}
		if activeDuration > c.TTLMax {
			activeDuration = c.TTLMax
		}

		profile := &HotKeyProfile{
			Key:               stats.Key,
			HitCountLastMonth: stats.TotalHits,
			AvgFirstHour:      avgHour,
			AvgFirstMinute:    avgMinute,
			SuggestedTTL:      activeDuration,
			ActiveDurationSec: activeDuration.Seconds(),
			EvalMonth:         monthLabel(prevYear, prevMonth),
		}
		newProfiles[stats.Key] = profile
		snapshots = append(snapshots, *profile)

		log.Printf("[Cache Eval] HOT KEY: key=%-25q | hits=%d | pre-warm=%02d:%02d | TTL=%v",
			stats.Key,
			stats.TotalHits,
			avgHour, avgMinute,
			activeDuration.Round(time.Second),
		)
	}

	// Simpan snapshot ke riwayat
	c.evalHistory = append(c.evalHistory, MonthlyEvalSnapshot{
		Month:        monthLabel(prevYear, prevMonth),
		HotKeysFound: len(newProfiles),
		Profiles:     snapshots,
		EvaluatedAt:  now,
	})
	if len(c.evalHistory) > 12 {
		c.evalHistory = c.evalHistory[len(c.evalHistory)-12:]
	}

	// Terapkan hot key profiles baru untuk bulan ini
	c.hotKeyProfiles = newProfiles

	// Reset bulan aktif
	c.currentMonth = int(now.Month())
	c.currentYear = now.Year()

	log.Printf("[Cache Eval] %d hot key terdaftar untuk bulan %s.",
		len(newProfiles), monthLabel(c.currentYear, c.currentMonth))
	log.Printf("[Cache Eval] Pre-warming terjadwal sesuai AvgFirstAccess masing-masing key.")
}

// WarmupScheduler memeriksa setiap menit apakah ada hot key yang perlu di-warm.
//
// Pilar 1 beraksi di sini:
//   Jika jam sekarang cocok dengan AvgFirstHour:AvgFirstMinute suatu hot key
//   dan belum di-warm hari ini → panggil WarmupFunc → simpan ke cache
//   → Pengguna datang, cache sudah siap!
func (c *AdaptiveCache) WarmupScheduler() {
	ticker := time.NewTicker(WarmupCheckInterval)
	defer ticker.Stop()
	log.Println("[Cache] WarmupScheduler goroutine aktif (cek tiap 1 menit).")
	for range ticker.C {
		c.checkAndWarm()
	}
}

func (c *AdaptiveCache) checkAndWarm() {
	c.mu.Lock()
	fn := c.warmupFunc
	c.mu.Unlock()

	if fn == nil {
		return // warmup function belum didaftarkan
	}

	now := time.Now()
	today := now.Format("2006-01-02")

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, profile := range c.hotKeyProfiles {
		// Sudah di-warm hari ini? Lewati.
		if profile.lastWarmDate == today {
			continue
		}

		// Cek apakah jam sekarang cocok dengan jadwal pre-warm
		if now.Hour() == profile.AvgFirstHour && now.Minute() == profile.AvgFirstMinute {
			key := profile.Key
			log.Printf("[Cache PREWARM] ⏰ Jadwal terpicu! key=%q | jadwal=%02d:%02d | TTL=%v",
				key, profile.AvgFirstHour, profile.AvgFirstMinute,
				profile.SuggestedTTL.Round(time.Second))

			// Panggil WarmupFunc di goroutine terpisah (tidak block scheduler)
			go func(k string, ttl time.Duration, pf *HotKeyProfile) {
				data, err := fn(k)
				if err != nil {
					log.Printf("[Cache PREWARM] GAGAL pre-warm key=%q: %v", k, err)
					return
				}
				c.mu.Lock()
				defer c.mu.Unlock()

				now2 := time.Now()
				stats := &keyMonthlyStats{
					Key:             k,
					Month:           c.currentMonth,
					Year:            c.currentYear,
					FirstAccessEver: now2,
					LastAccessEver:  now2,
					TotalHits:       0,
					DailyRecords: map[string]*dailyRecord{
						now2.Format("2006-01-02"): {
							Date:      now2.Format("2006-01-02"),
							FirstTime: now2,
							LastTime:  now2,
							Hits:      0,
						},
					},
				}
				c.items[k] = &CacheEntry{
					Value:    data,
					ExpireAt: now2.Add(ttl),
					stats:    stats,
				}
				pf.lastWarmDate = today
				log.Printf("[Cache PREWARM] ✅ key=%q berhasil di-pre-warm. TTL=%v | expire=%s",
					k, ttl.Round(time.Second), now2.Add(ttl).Format("15:04:05"))
			}(key, profile.SuggestedTTL, profile)
		}
	}
}

// TriggerEvaluationNow memaksa evaluasi bulanan sekarang (untuk testing).
func (c *AdaptiveCache) TriggerEvaluationNow() {
	log.Println("[Cache] TriggerEvaluationNow: evaluasi dipicu manual.")
	c.runMonthlyEvaluation(time.Now())
}

// TriggerWarmupNow memaksa pre-warm semua hot key sekarang (untuk testing).
func (c *AdaptiveCache) TriggerWarmupNow() {
	c.mu.Lock()
	// Reset flag last_warm_date agar semua hot key bisa di-warm ulang
	for _, p := range c.hotKeyProfiles {
		p.lastWarmDate = ""
	}
	fn := c.warmupFunc
	c.mu.Unlock()

	if fn == nil {
		log.Println("[Cache] TriggerWarmupNow: WarmupFunc belum didaftarkan.")
		return
	}
	log.Println("[Cache] TriggerWarmupNow: memicu pre-warm semua hot key.")
	c.checkAndWarm()
}

// ── Statistik ──────────────────────────────────────────────────────────────────

// GetStats mengembalikan statistik lengkap sistem cache adaptif.
func (c *AdaptiveCache) GetStats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()
	total := c.TotalHits + c.TotalMisses
	hitRatio := 0.0
	if total > 0 {
		hitRatio = float64(c.TotalHits) / float64(total) * 100.0
	}

	// Statistik per key aktif
	activeStats := []ActiveKeyStat{}
	activeCount := 0
	for key, entry := range c.items {
		if now.Before(entry.ExpireAt) {
			activeCount++
			s := entry.stats
			dur := 0.0
			avgH, avgM := 0, 0
			if s != nil {
				dur = s.LastAccessEver.Sub(s.FirstAccessEver).Seconds()
				avgH, avgM = computeAvgFirstAccess(s.DailyRecords)
				activeStats = append(activeStats, ActiveKeyStat{
					Key:               key,
					HitCount:          s.TotalHits,
					ActiveDurationSec: dur,
					AvgFirstHour:      avgH,
					AvgFirstMinute:    avgM,
					TTLRemainingSec:   time.Until(entry.ExpireAt).Seconds(),
				})
			}
		}
	}

	// Hot key profiles (untuk bulan depan)
	profiles := []HotKeyProfile{}
	for _, p := range c.hotKeyProfiles {
		profiles = append(profiles, *p)
	}

	return CacheStats{
		ItemCount:      activeCount,
		TotalHits:      c.TotalHits,
		TotalMisses:    c.TotalMisses,
		HitRatioPct:    hitRatio,
		CurrentMonth:   monthLabel(c.currentYear, c.currentMonth),
		TTLBaselineSec: c.TTLBaseline.Seconds(),
		TTLMaxSec:      c.TTLMax.Seconds(),
		AdaptCoeff:     c.AdaptCoeff,
		CleanupCount:   c.CleanupCount,
		HotKeyProfiles: profiles,
		MonthlyHistory: c.evalHistory,
		ActiveKeyStats: activeStats,
	}
}

// ── Helper ─────────────────────────────────────────────────────────────────────

// computeAvgFirstAccess menghitung rata-rata jam & menit akses pertama harian.
func computeAvgFirstAccess(daily map[string]*dailyRecord) (hour, minute int) {
	if len(daily) == 0 {
		return 0, 0
	}
	var totalMinutes int
	n := 0
	for _, rec := range daily {
		totalMinutes += rec.FirstTime.Hour()*60 + rec.FirstTime.Minute()
		n++
	}
	avgMin := totalMinutes / n
	return avgMin / 60, avgMin % 60
}

func monthKey(year, month int) string {
	return fmt.Sprintf("%04d-%02d", year, month)
}

func monthLabel(year, month int) string {
	return time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC).Format("January 2006")
}
