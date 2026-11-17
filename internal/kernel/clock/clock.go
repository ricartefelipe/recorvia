// Package clock fornece um relógio injetável para regras temporais.
package clock

import (
	"sync"
	"time"
)

// Clock informa o instante atual. Regras de negócio recebem um Clock em vez
// de chamar time.Now diretamente, permitindo testes determinísticos.
type Clock interface {
	Now() time.Time
}

// System usa o relógio do sistema e sempre retorna UTC.
type System struct{}

// Now retorna o instante atual em UTC.
func (System) Now() time.Time { return time.Now().UTC() }

// Manual é um relógio controlado pelo teste, seguro para uso concorrente.
type Manual struct {
	mu  sync.Mutex
	now time.Time
}

// NewManual cria um relógio manual no instante t.
func NewManual(t time.Time) *Manual { return &Manual{now: t.UTC()} }

// Now retorna o instante configurado.
func (m *Manual) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// Set redefine o instante.
func (m *Manual) Set(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = t.UTC()
}

// Advance avança o relógio em d.
func (m *Manual) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = m.now.Add(d)
}
