package models

// Product adalah entitas utama dalam sistem
type Product struct {
	ID    int64   `db:"id"    json:"id"`
	Name  string  `db:"name"  json:"name"`
	Price float64 `db:"price" json:"price"`
}

// CreateProductRequest adalah struct validasi untuk POST /api/products
type CreateProductRequest struct {
	Name  string  `json:"name"  binding:"required,min=2"`
	Price float64 `json:"price" binding:"required,gt=0"`
}

// UpdateProductRequest adalah struct validasi untuk PUT /api/products/:id
type UpdateProductRequest struct {
	Name  string  `json:"name"  binding:"required,min=2"`
	Price float64 `json:"price" binding:"required,gt=0"`
}
