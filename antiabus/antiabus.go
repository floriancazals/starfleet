// Package antiabus protège les portes publiques d'une app — la connexion, un
// formulaire ouvert à tous — sans captcha : un budget de tentatives par adresse,
// et la règle qui dit quelle est l'adresse.
package antiabus

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ClientIP rend l'adresse du visiteur, celle que comptent les budgets. Elle se
// lit dans X-Real-Ip — pas dans X-Forwarded-For.
//
// Les apps tournent derrière un ou deux Traefik : un seul sur le VPS et le Pi 4,
// deux pour le Pi 5, qui est derrière la porte d'entrée. X-Forwarded-For y porte
// alors « <vrai client>, <Pi 4> », et aucune des deux règles habituelles ne
// marche partout : la dernière entrée est le proxy interne, la première ne vaut
// que parce que la porte d'entrée jette ce que le client a forgé. X-Real-Ip ne
// porte qu'une valeur, et c'est la bonne quel que soit le nombre de relais : un
// Traefik efface celle d'un pair auquel il ne fait pas confiance et la réécrit
// (mesuré au banc le 11/09/2026, deux traefik:v3.7.4 chaînés).
//
// Le piège est que la MAUVAISE règle ne ressemble pas à une panne : tous les
// visiteurs se retrouvent sous une seule clé, et un budget « par adresse »
// devient un compteur unique partagé par la terre entière.
//
// Sans proxy devant (un banc local), c'est l'adresse de la connexion.
func ClientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); net.ParseIP(ip) != nil {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Limiter est un budget de tentatives par clé (une adresse, en général), sur une
// fenêtre glissante, en mémoire. Il émousse les doubles envois et l'abus
// ordinaire d'une porte publique ; ce n'est pas une défense contre un attaquant
// déterminé. Un Limiter nil laisse tout passer.
type Limiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	max    int
	window time.Duration
}

// NewLimiter accepte maxHits tentatives par clé et par fenêtre.
func NewLimiter(maxHits int, window time.Duration) *Limiter {
	return &Limiter{hits: make(map[string][]time.Time), max: maxHits, window: window}
}

// Allow compte une tentative pour key et dit si elle passe. Une tentative
// refusée n'est pas comptée.
func (l *Limiter) Allow(key string) bool {
	if l == nil {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)
	kept := l.prune(key, now)
	if len(kept) >= l.max {
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// Allowed dit si key est sous la limite, SANS compter de tentative. Associé à
// Record, il ne fait payer que les tentatives qui comptent (une connexion
// ratée) : c'est ce qui empêche une administratrice de s'enfermer dehors à force
// de se connecter.
func (l *Limiter) Allowed(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.prune(key, time.Now())) < l.max
}

// Record compte une tentative pour key.
func (l *Limiter) Record(key string) {
	if l == nil {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)
	l.hits[key] = append(l.prune(key, now), now)
}

// prune ôte les tentatives de key sorties de la fenêtre et rend les autres.
// L'appelant tient le verrou.
func (l *Limiter) prune(key string, now time.Time) []time.Time {
	cutoff := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, key)
		return nil
	}
	l.hits[key] = kept
	return kept
}

// sweep efface les clés périmées quand la table grossit, pour qu'elle ne
// grandisse pas sans fin. L'appelant tient le verrou.
func (l *Limiter) sweep(now time.Time) {
	if len(l.hits) <= 1024 {
		return
	}
	cutoff := now.Add(-l.window)
	for k, ts := range l.hits {
		if len(ts) == 0 || ts[len(ts)-1].Before(cutoff) {
			delete(l.hits, k)
		}
	}
}
