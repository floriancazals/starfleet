package antiabus

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestLimiterBloqueAuDelaDuBudget(t *testing.T) {
	l := NewLimiter(3, time.Minute)
	for i := range 3 {
		if !l.Allow("10.0.0.1") {
			t.Fatalf("tentative %d refusée", i+1)
		}
	}
	if l.Allow("10.0.0.1") {
		t.Error("la 4e tentative doit être refusée")
	}
	if !l.Allow("10.0.0.2") {
		t.Error("une autre adresse a son propre budget")
	}
}

func TestLimiterOublieLesTentativesPerimees(t *testing.T) {
	l := NewLimiter(1, 20*time.Millisecond)
	if !l.Allow("10.0.0.1") {
		t.Fatal("la première tentative doit passer")
	}
	if l.Allow("10.0.0.1") {
		t.Fatal("la deuxième doit être refusée dans la fenêtre")
	}
	time.Sleep(30 * time.Millisecond)
	if !l.Allow("10.0.0.1") {
		t.Error("une fois la fenêtre passée, la tentative doit passer")
	}
}

// Allowed ne consomme pas le budget : seul Record le fait.
func TestAllowedNeComptePas(t *testing.T) {
	l := NewLimiter(1, time.Minute)
	for range 5 {
		if !l.Allowed("10.0.0.1") {
			t.Fatal("Allowed ne doit pas consommer le budget")
		}
	}
	l.Record("10.0.0.1")
	if l.Allowed("10.0.0.1") {
		t.Error("le budget doit être épuisé après Record")
	}
}

func TestUnLimiterNilLaissePasser(t *testing.T) {
	var l *Limiter
	if !l.Allow("x") || !l.Allowed("x") {
		t.Error("un Limiter nil ne doit rien bloquer")
	}
	l.Record("x") // ne doit pas paniquer
}

// La table ne grandit pas sans fin : passé un millier de clés, les périmées partent.
func TestLaTableSeVide(t *testing.T) {
	l := NewLimiter(1, time.Minute)
	vieux := time.Now().Add(-time.Hour)
	for i := range 2000 {
		l.hits["10.0."+strconv.Itoa(i/256)+"."+strconv.Itoa(i%256)] = []time.Time{vieux}
	}
	l.Record("10.9.9.9")
	if n := len(l.hits); n != 1 {
		t.Errorf("%d clés gardées, attendu seulement la neuve", n)
	}
}

func TestClientIP(t *testing.T) {
	cas := []struct {
		nom, realIP, xff, remote, veut string
	}{
		{"sans proxy", "", "", "203.0.113.7:54321", "203.0.113.7"},
		// Derrière les deux Traefik du Pi 5 : X-Forwarded-For se termine par le
		// Pi 4, et c'est X-Real-Ip qui porte le vrai client.
		{"deux proxys", "203.0.113.7", "203.0.113.7, 192.168.1.3", "172.18.0.2:1234", "203.0.113.7"},
		{"un proxy", "203.0.113.7", "203.0.113.7", "172.18.0.2:1234", "203.0.113.7"},
		{"x-real-ip illisible", "pas une adresse", "", "203.0.113.7:1", "203.0.113.7"},
		{"ipv6", "2001:db8::1", "", "[::1]:1", "2001:db8::1"},
		{"remote sans port", "", "", "203.0.113.7", "203.0.113.7"},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			r := &http.Request{Header: http.Header{}, RemoteAddr: c.remote}
			if c.realIP != "" {
				r.Header.Set("X-Real-Ip", c.realIP)
			}
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := ClientIP(r); got != c.veut {
				t.Errorf("ClientIP = %q, attendu %q", got, c.veut)
			}
		})
	}
}
