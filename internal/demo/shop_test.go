package demo

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShopCartSurvivesAcrossBackends(t *testing.T) {
	a, _ := Start("api-1", "127.0.0.1:0", Chaos{})
	defer a.Close()
	b, _ := Start("api-2", "127.0.0.1:0", Chaos{})
	defer b.Close()

	get := func(base, path string, cookies []*http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Accept", "text/html")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		map[string]*Server{"a": a, "b": b}[base].Handler().ServeHTTP(rec, req)
		return rec
	}

	add := get("a", "/cart/add?id=3", nil)
	if add.Code != http.StatusSeeOther {
		t.Fatalf("add: got %d", add.Code)
	}
	cookies := add.Result().Cookies()
	add = get("a", "/cart/add?id=3", cookies)
	cookies = add.Result().Cookies()

	cart := get("b", "/cart", cookies).Body.String()
	if !strings.Contains(cart, "Served by api-2") || !strings.Contains(cart, "Rs. 900") {
		t.Fatalf("cart on the other backend should show 2 bottles, got:\n%s", cart)
	}
	if code := get("b", "/product/99", nil).Code; code != 404 {
		t.Fatalf("missing product: got %d", code)
	}
}
