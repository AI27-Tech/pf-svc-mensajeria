package bandejas_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	bandejashttp "github.com/AI27-Tech/pf-svc-mensajeria/adapters/http/bandejas"
)

// trabajadorPG falla como fallaba Postgres con un sujeto que no es UUID. Si el
// handler llega a llamarlo, el texto de la causa queda disponible para filtrarse.
type trabajadorPG struct{ llamado bool }

func (f *trabajadorPG) TrabajoChat(context.Context) ([]msgclient.Conversacion, int64, error) {
	f.llamado = true
	return nil, 0, errors.New(`ERROR: invalid input syntax for type uuid: "no-es-uuid" (SQLSTATE 22P02)`)
}
func (f *trabajadorPG) CambiosChatDesde(context.Context, int64) ([]msgclient.Conversacion, int64, bool, error) {
	panic("no se invoca")
}
func (f *trabajadorPG) Trabajo(context.Context, string, string) ([]msgclient.Conversacion, int64, error) {
	panic("no se invoca")
}
func (f *trabajadorPG) CambiosDesde(context.Context, string, string, int64) ([]msgclient.Conversacion, int64, bool, error) {
	panic("no se invoca")
}

// TestMiTrabajoSujetoNoUUID reproduce el filtrado medido: un token con sujeto no
// UUID respondía 502 con el texto de Postgres en el cuerpo. Ahora se rechaza en la
// frontera, antes de SQL, con el mismo 401 que la ausencia de claims.
func TestMiTrabajoSujetoNoUUID(t *testing.T) {
	trab := &trabajadorPG{}
	mux := http.NewServeMux()
	bandejashttp.Handlers{Bandeja: trab}.Montar(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/bandeja/trabajo", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{
		Subject: "no-es-uuid", TenantID: "t1", Rol: "ejecutivo", Audiencia: auth.AudienciaInterno,
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
	if want := `{"codigo":"no_autorizado","mensaje":"sin claims"}` + "\n"; rec.Body.String() != want {
		t.Errorf("cuerpo:\n got %q\nwant %q", rec.Body.String(), want)
	}
	if trab.llamado {
		t.Error("el sujeto no UUID llegó al caso de uso (SQL)")
	}
}
