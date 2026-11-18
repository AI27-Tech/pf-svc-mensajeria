package alcance

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAcota fija qué roles operan sobre las emergencias del viaje solo dentro
// de sus clientes (mensajeria.yaml, DEC-149 #6): el monitoreo. El administrador
// ve todo y Reacción no cambia.
func TestAcota(t *testing.T) {
	quiere := map[string]bool{
		"monitorista": true, "supervisor": true, "coordinador": true,
		"administrador": false, "reaccion": false, "ejecutivo": false,
		"jefe_operativo": false, "cliente": false, "": false,
	}
	for rol, q := range quiere {
		if got := Acota(rol); got != q {
			t.Errorf("Acota(%q) = %v; quiere %v", rol, got, q)
		}
	}
}

// TestVisible: el cliente del viaje en el conjunto; vacío, "" o no UUID no ven
// nada. En un préstamo decide el cliente del viaje (DEC-149 #5).
func TestVisible(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	casos := []struct {
		nombre   string
		clientes []uuid.UUID
		cliente  string
		quiere   bool
	}{
		{"su cliente", []uuid.UUID{a}, a.String(), true},
		{"unión de clientes (supervisor)", []uuid.UUID{b, a}, a.String(), true},
		{"otro cliente", []uuid.UUID{b}, a.String(), false},
		{"sin alcance (nil)", nil, a.String(), false},
		{"sin alcance (vacío)", []uuid.UUID{}, a.String(), false},
		{"caso sin cliente_id", []uuid.UUID{a}, "", false},
		{"cliente_id ilegible", []uuid.UUID{a}, "x", false},
	}
	for _, c := range casos {
		if got := Visible(c.clientes, c.cliente); got != c.quiere {
			t.Errorf("%s: Visible = %v; quiere %v", c.nombre, got, c.quiere)
		}
	}
}

// TestLeerPayload fija el parseo de alcance.cambiado tal como lo publica
// identidad; lo ilegible se descarta.
func TestLeerPayload(t *testing.T) {
	c := uuid.New()
	a, ok := leer([]byte(`{"sujeto_id":"11111111-1111-1111-1111-111111111111","idp_subject":"s-1","clientes_efectivos_ids":["` + c.String() + `"],"cambiado_en":"2026-11-17T10:00:00.123456Z"}`))
	if !ok || a.IdpSubject != "s-1" || len(a.Clientes) != 1 || a.Clientes[0] != c ||
		!a.CambiadoEn.Equal(time.Date(2026, 11, 17, 10, 0, 0, 123456000, time.UTC)) {
		t.Errorf("payload válido: %+v ok=%v", a, ok)
	}
	if a, ok := leer([]byte(`{"sujeto_id":"11111111-1111-1111-1111-111111111111","idp_subject":"s-1","clientes_efectivos_ids":[],"cambiado_en":"2026-11-17T10:00:00Z"}`)); !ok || a.Clientes == nil || len(a.Clientes) != 0 {
		t.Errorf("vacío = quitar: %+v ok=%v; quiere lista vacía", a, ok)
	}
	for _, malo := range []string{
		`no json`,
		`{"clientes_efectivos_ids":[],"cambiado_en":"2026-11-17T10:00:00Z"}`,
		// Un alcance de tracer.41 sin idp_subject no se puede indexar (DEC-149 #22).
		`{"sujeto_id":"11111111-1111-1111-1111-111111111111","clientes_efectivos_ids":[],"cambiado_en":"2026-11-17T10:00:00Z"}`,
		`{"sujeto_id":"11111111-1111-1111-1111-111111111111","idp_subject":"","clientes_efectivos_ids":[],"cambiado_en":"2026-11-17T10:00:00Z"}`,
		`{"sujeto_id":"11111111-1111-1111-1111-111111111111","idp_subject":"s-1","clientes_efectivos_ids":[]}`,
		`{"sujeto_id":"11111111-1111-1111-1111-111111111111","idp_subject":"s-1","clientes_efectivos_ids":["x"],"cambiado_en":"2026-11-17T10:00:00Z"}`,
	} {
		if _, ok := leer([]byte(malo)); ok {
			t.Errorf("payload ilegible aceptado: %s", malo)
		}
	}
}
