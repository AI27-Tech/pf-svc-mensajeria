package boundary

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
)

// causaPG imita el texto que Postgres devuelve ante un UUID mal formado: es lo
// que NO debe llegar al cuerpo de la respuesta.
var causaPG = errors.New(`ERROR: invalid input syntax for type uuid: "no-es-uuid" (SQLSTATE 22P02)`)

func TestWriteError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "roto con causa: solo el mensaje público",
			err:        platformerrors.NewRotoErr(causaPG, "mensajeria: error interno"),
			wantStatus: 502,
			wantBody:   `{"codigo":"roto","mensaje":"mensajeria: error interno"}` + "\n",
		},
		{
			name:       "roto envuelto con %w: el PlatformError más externo",
			err:        fmt.Errorf("listar: %w", platformerrors.NewRotoErr(causaPG, "autorizacion: verificar conversacion")),
			wantStatus: 502,
			wantBody:   `{"codigo":"roto","mensaje":"autorizacion: verificar conversacion"}` + "\n",
		},
		{
			name:       "error fuera de la taxonomía: sin texto interno",
			err:        causaPG,
			wantStatus: 500,
			wantBody:   `{"codigo":"desconocido","mensaje":"error interno"}` + "\n",
		},
		{
			name:       "conflicto sin causa: su mensaje",
			err:        platformerrors.NewConflicto("conflicto de versión del reclamo"),
			wantStatus: 409,
			wantBody:   `{"codigo":"conflicto","mensaje":"conflicto de versión del reclamo"}` + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteError(rec, tt.err)
			if rec.Code != tt.wantStatus {
				t.Errorf("status: got %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("cuerpo:\n got %q\nwant %q", got, tt.wantBody)
			}
		})
	}
}

func TestClaims(t *testing.T) {
	const sujeto = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	tests := []struct {
		name    string
		claims  *auth.Claims
		wantNil bool
	}{
		{"sin claims", nil, true},
		{"sujeto no UUID", &auth.Claims{Subject: "no-es-uuid", TenantID: "t1"}, true},
		{"sujeto vacío", &auth.Claims{Subject: "", TenantID: "t1"}, true},
		{"sujeto UUID con prefijo urn", &auth.Claims{Subject: "urn:uuid:" + sujeto, TenantID: "t1"}, true},
		{"sujeto UUID", &auth.Claims{Subject: sujeto, TenantID: "t1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.claims != nil {
				ctx = auth.ContextWithClaims(ctx, tt.claims)
			}
			got := Claims(ctx)
			if (got == nil) != tt.wantNil {
				t.Fatalf("Claims: got %+v, wantNil %v", got, tt.wantNil)
			}
			if got != nil && got.Subject != sujeto {
				t.Errorf("sujeto: got %q, want %q", got.Subject, sujeto)
			}
		})
	}
}
