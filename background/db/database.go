// Package db mengelola koneksi SQLite dan operasi CRUD produk.
// Menggunakan modernc.org/sqlite (pure Go, tidak perlu CGO/gcc).
package db

import (
	"log"
	"smart-api/background/models"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite" // driver: "sqlite"
)

// DB membungkus sqlx.DB untuk kemudahan penggunaan
type DB struct {
	*sqlx.DB
}

// InitDB membuka koneksi SQLite, membuat tabel products jika belum ada,
// dan menyemai data awal jika tabel masih kosong.
func InitDB(path string) (*DB, error) {
	db, err := sqlx.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		return nil, err
	}

	// Aktifkan WAL mode untuk performa concurrent baca/tulis
	db.Exec("PRAGMA journal_mode=WAL;")
	db.Exec("PRAGMA foreign_keys=ON;")

	// Buat tabel products
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS products (
			id    INTEGER PRIMARY KEY AUTOINCREMENT,
			name  TEXT    NOT NULL,
			price REAL    NOT NULL CHECK(price > 0)
		)
	`)
	if err != nil {
		return nil, err
	}

	wrapped := &DB{db}

	// Semai data awal jika tabel kosong
	var count int
	db.QueryRow("SELECT COUNT(*) FROM products").Scan(&count)
	if count == 0 {
		log.Println("[DB] Menyemai data produk awal...")
		seeds := []models.Product{
			{Name: "Laptop Lenovo IdeaPad", Price: 8_500_000},
			{Name: "Mouse Wireless Logitech MX Master", Price: 850_000},
			{Name: "Keyboard Mechanical Keychron K2", Price: 1_200_000},
			{Name: "Monitor 24\" Full HD Dell", Price: 2_750_000},
			{Name: "USB Hub 7-Port Anker", Price: 285_000},
			{Name: "Webcam Logitech C920 HD", Price: 1_100_000},
			{Name: "SSD External Samsung 1TB", Price: 1_650_000},
			{Name: "Headset Gaming HyperX Cloud", Price: 950_000},
		}
		for _, p := range seeds {
			wrapped.CreateProduct(&p)
		}
		log.Printf("[DB] %d produk berhasil disemai.", len(seeds))
	}

	log.Printf("[DB] Koneksi ke %s berhasil.", path)
	return wrapped, nil
}

// ── Operasi CRUD ───────────────────────────────────────────────────────────────

// GetAllProducts mengambil seluruh produk dari database.
func (d *DB) GetAllProducts() ([]models.Product, error) {
	var products []models.Product
	err := d.Select(&products, "SELECT id, name, price FROM products ORDER BY id ASC")
	return products, err
}

// GetProductByID mengambil satu produk berdasarkan ID.
// Mengembalikan nil jika tidak ditemukan.
func (d *DB) GetProductByID(id int64) (*models.Product, error) {
	var product models.Product
	err := d.Get(&product, "SELECT id, name, price FROM products WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	return &product, nil
}

// CreateProduct menyimpan produk baru ke database.
// ID akan diisi otomatis dari LastInsertId.
func (d *DB) CreateProduct(p *models.Product) error {
	result, err := d.Exec(
		"INSERT INTO products (name, price) VALUES (?, ?)",
		p.Name, p.Price,
	)
	if err != nil {
		return err
	}
	p.ID, err = result.LastInsertId()
	return err
}

// UpdateProduct memperbarui nama dan harga produk berdasarkan ID.
func (d *DB) UpdateProduct(id int64, name string, price float64) error {
	_, err := d.Exec(
		"UPDATE products SET name = ?, price = ? WHERE id = ?",
		name, price, id,
	)
	return err
}

// DeleteProduct menghapus produk berdasarkan ID.
func (d *DB) DeleteProduct(id int64) error {
	_, err := d.Exec("DELETE FROM products WHERE id = ?", id)
	return err
}

// ProductExists mengecek apakah produk dengan ID tertentu ada.
func (d *DB) ProductExists(id int64) bool {
	var count int
	d.QueryRow("SELECT COUNT(*) FROM products WHERE id = ?", id).Scan(&count)
	return count > 0
}
