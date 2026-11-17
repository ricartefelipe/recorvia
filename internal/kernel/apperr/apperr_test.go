package apperr

import (
	"errors"
	"fmt"
	"testing"
)

func TestConstructorsAndKinds(t *testing.T) {
	cases := []struct {
		err  *Error
		kind Kind
		name string
	}{
		{Invalid("c", "m", FieldError{Field: "f", Message: "x"}), KindInvalid, "invalid"},
		{NotFound("c", "m"), KindNotFound, "not_found"},
		{Conflict("c", "m"), KindConflict, "conflict"},
		{Forbidden("c", "m"), KindForbidden, "forbidden"},
		{Unauthenticated("c", "m"), KindUnauthenticated, "unauthenticated"},
		{PreconditionFailed("c", "m"), KindPreconditionFailed, "precondition_failed"},
		{Unavailable("c", "m"), KindUnavailable, "unavailable"},
		{RateLimited("c", "m"), KindRateLimited, "rate_limited"},
		{LimitExceeded("c", "m"), KindLimitExceeded, "limit_exceeded"},
	}
	for _, tc := range cases {
		if tc.err.Kind != tc.kind || tc.kind.String() != tc.name {
			t.Errorf("%s: kind=%v", tc.name, tc.err.Kind)
		}
		wrapped := fmt.Errorf("contexto: %w", tc.err)
		if KindOf(wrapped) != tc.kind || !IsKind(wrapped, tc.kind) {
			t.Errorf("%s: KindOf falhou através de wrap", tc.name)
		}
	}
	if KindInternal.String() != "internal" || KindOf(errors.New("x")) != KindInternal {
		t.Fatal("erros desconhecidos devem ser internos")
	}
}

func TestWrapIsAndMessages(t *testing.T) {
	cause := errors.New("falha de banco")
	base := Conflict("invoice_not_open", "A fatura não está em aberto.")
	e := base.Wrap(cause)
	if !errors.Is(e, cause) {
		t.Fatal("causa deve ser acessível")
	}
	if !errors.Is(e, &Error{Kind: KindConflict}) || !errors.Is(e, &Error{Kind: KindConflict, Code: "invoice_not_open"}) {
		t.Fatal("Is por categoria/código falhou")
	}
	if errors.Is(e, &Error{Kind: KindConflict, Code: "outro"}) || errors.Is(e, cause) == false {
		t.Fatal("Is não deve casar código diferente")
	}
	if e.Error() != "invoice_not_open: A fatura não está em aberto.: falha de banco" {
		t.Fatalf("Error()=%q", e.Error())
	}
	if base.Error() != "invoice_not_open: A fatura não está em aberto." {
		t.Fatalf("Error() sem causa=%q", base.Error())
	}
	if base.Err != nil {
		t.Fatal("Wrap não deve mutar o original")
	}
	got, ok := As(fmt.Errorf("x: %w", e))
	if !ok || got.Code != "invoice_not_open" {
		t.Fatal("As falhou")
	}
	if _, ok := As(cause); ok {
		t.Fatal("As não deveria encontrar *Error")
	}
	if (&Error{}).Is(cause) {
		t.Fatal("Is com alvo de outro tipo deve ser falso")
	}
}
