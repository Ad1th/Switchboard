package demo

import (
	"html/template"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
)

// A tiny shop that every demo backend serves. The cart lives in a cookie,
// so it survives each page being served by a different backend.

type product struct {
	ID    int
	Name  string
	Price int // rupees
	About string
}

var products = []product{
	{1, "Notebook, A5 ruled", 120, "200 pages, stitched binding."},
	{2, "Ball pen, blue (pack of 5)", 50, "Smooth ink, medium tip."},
	{3, "Steel water bottle, 1 L", 450, "Keeps water cold for 12 hours."},
	{4, "Backpack, 25 L", 1299, "Two compartments and a laptop sleeve."},
	{5, "Desk lamp", 899, "LED, three brightness levels."},
	{6, "USB-C cable, 1 m", 249, "Charges and transfers data."},
	{7, "Coffee mug", 199, "Ceramic, 300 ml."},
	{8, "Geometry box", 150, "Compass, divider, protractor and scales."},
}

func findProduct(id int) (product, bool) {
	i := slices.IndexFunc(products, func(p product) bool { return p.ID == id })
	if i < 0 {
		return product{}, false
	}
	return products[i], true
}

type cartLine struct {
	product
	Qty   int
	Total int
}

func readCart(r *http.Request) map[int]int {
	cart := map[int]int{}
	c, err := r.Cookie("cart")
	if err != nil {
		return cart
	}
	for _, part := range strings.Split(c.Value, ".") {
		if id, err := strconv.Atoi(part); err == nil {
			if _, ok := findProduct(id); ok {
				cart[id]++
			}
		}
	}
	return cart
}

func writeCart(w http.ResponseWriter, cart map[int]int) {
	var parts []string
	for _, p := range products {
		for range cart[p.ID] {
			parts = append(parts, strconv.Itoa(p.ID))
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "cart", Value: strings.Join(parts, "."), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func cartCount(cart map[int]int) int {
	n := 0
	for _, q := range cart {
		n += q
	}
	return n
}

var shopTmpl = template.Must(template.New("shop").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} - Corner Store</title>
<link rel="icon" href="data:,">
<style>
body { margin: 0; font-family: Georgia, serif; }
.served { background: {{.Color}}; color: #fff; padding: 4px 10px; font: 13px monospace; }
.page { max-width: 720px; margin: 0 auto; padding: 0 12px 30px; }
table { border-collapse: collapse; width: 100%; }
td, th { border: 1px solid #999; padding: 4px 6px; text-align: left; }
.r { text-align: right; }
</style></head>
<body>
<div class="served">Served by {{.Backend}}, port {{.Port}}, pid {{.PID}}</div>
<div class="page">
<h1>Corner Store</h1>
<p><a href="/">Products</a> | <a href="/cart">Cart ({{.CartCount}})</a> | <a href="/backend">About this server</a></p>
<hr>
{{if eq .View "list"}}
<h2>Products</h2>
<table>
<tr><th>Item</th><th class="r">Price</th><th></th></tr>
{{range .Products}}<tr><td><a href="/product/{{.ID}}">{{.Name}}</a></td><td class="r">Rs. {{.Price}}</td><td><a href="/cart/add?id={{.ID}}">Add to cart</a></td></tr>
{{end}}</table>
{{else if eq .View "product"}}
<h2>{{.Product.Name}}</h2>
<p>{{.Product.About}}</p>
<p>Price: Rs. {{.Product.Price}}</p>
<p><a href="/cart/add?id={{.Product.ID}}">Add to cart</a></p>
{{else if eq .View "cart"}}
<h2>Your cart</h2>
{{if .Lines}}
<table>
<tr><th>Item</th><th class="r">Qty</th><th class="r">Price</th><th class="r">Total</th></tr>
{{range .Lines}}<tr><td>{{.Name}}</td><td class="r">{{.Qty}}</td><td class="r">Rs. {{.Price}}</td><td class="r">Rs. {{.Total}}</td></tr>
{{end}}<tr><th colspan="3">Total</th><th class="r">Rs. {{.Total}}</th></tr>
</table>
<p><a href="/cart/clear">Empty cart</a></p>
<p><small>The cart is kept in a cookie, so it stays the same whichever server answers.</small></p>
{{else}}<p>Your cart is empty. <a href="/">Browse products</a>.</p>{{end}}
{{else}}
<h2>Page not found</h2>
<p><a href="/">Back to products</a></p>
{{end}}
</div>
</body></html>`))

func (s *Server) shop(w http.ResponseWriter, r *http.Request, served int64) {
	cart := readCart(r)
	data := map[string]any{
		"Backend": s.Name,
		"Color":   template.CSS(Color(s.Name)),
		"Port":    s.port(),
		"PID":     os.Getpid(),
		"View":    "list",
		"Title":   "Products",
	}
	path := r.URL.Path
	switch {
	case path == "/":
		data["Products"] = products
	case path == "/backend":
		s.page(w, r, served)
		return
	case strings.HasPrefix(path, "/product/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(path, "/product/"))
		p, ok := findProduct(id)
		if !ok {
			data["View"], data["Title"] = "missing", "Not found"
			w.WriteHeader(http.StatusNotFound)
			break
		}
		data["View"], data["Title"], data["Product"] = "product", p.Name, p
	case path == "/cart/add":
		id, _ := strconv.Atoi(r.URL.Query().Get("id"))
		if _, ok := findProduct(id); ok {
			cart[id]++
			writeCart(w, cart)
		}
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	case path == "/cart/clear":
		writeCart(w, map[int]int{})
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	case path == "/cart":
		var lines []cartLine
		total := 0
		for _, p := range products {
			if q := cart[p.ID]; q > 0 {
				lines = append(lines, cartLine{p, q, q * p.Price})
				total += q * p.Price
			}
		}
		data["View"], data["Title"], data["Lines"], data["Total"] = "cart", "Cart", lines, total
	default:
		data["View"], data["Title"] = "missing", "Not found"
		w.WriteHeader(http.StatusNotFound)
	}
	data["CartCount"] = cartCount(cart)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	shopTmpl.Execute(w, data)
}
