package mensajeria

import (
	"context"
	"testing"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/emergencias"
	msgtipos "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
	"github.com/google/uuid"
)

// casoFake es un casoDeEmergencia doble: solo implementa ListarPorViaje con un
// caso fijo; el resto no se invoca en estas pruebas.
type casoFake struct {
	casos []emergencias.CasoProyectado
}

func (f *casoFake) ListarPorViaje(_ context.Context, _, _ string, _ bool) ([]emergencias.CasoProyectado, error) {
	return f.casos, nil
}
func (f *casoFake) Activar(context.Context, emergencias.EntradaActivacion) (emergencias.ResultadoActivacion, error) {
	panic("no se invoca")
}
func (f *casoFake) AgregarNota(context.Context, emergencias.EntradaNota) (string, error) {
	panic("no se invoca")
}
func (f *casoFake) Reclamar(context.Context, string, string, string, msgtipos.VersionReclamo) (msgtipos.VersionReclamo, error) {
	panic("no se invoca")
}
func (f *casoFake) Liberar(context.Context, string, string, string, string, msgtipos.VersionReclamo) error {
	panic("no se invoca")
}
func (f *casoFake) Transferir(context.Context, string, string, string, string, msgtipos.VersionReclamo) error {
	panic("no se invoca")
}
func (f *casoFake) Resolver(context.Context, string, string, string, msgtipos.VersionReclamo) error {
	panic("no se invoca")
}
func (f *casoFake) Cerrar(context.Context, string, string, string) error { panic("no se invoca") }

func ctxConAudiencia(aud auth.Audiencia) context.Context {
	return auth.ContextWithClaims(context.Background(), &auth.Claims{
		Subject:   "u-1",
		TenantID:  "t-1",
		Audiencia: aud,
	})
}

// unCaso es un caso interno con todos los campos poblados.
func unCaso() emergencias.CasoProyectado {
	return emergencias.CasoProyectado{
		ID:             uuid.New().String(),
		ViajeID:        uuid.New().String(),
		ClienteID:      uuid.New().String(),
		Estado:         "en_atencion",
		Origen:         "operacion",
		ActorReal:      uuid.New().String(),
		VersionReclamo: msgtipos.VersionReclamo(3),
		ActivadaEn:     time.Now().UTC(),
	}
}

// TestListarEmergencias_ClienteOcultaCamposInternos: para audiencia Cliente, la
// proyección NO expone origen ni actor_real (ADR 21 §6), pero sí el id/estado.
func TestListarEmergencias_ClienteOcultaCamposInternos(t *testing.T) {
	caso := unCaso()
	api := &emergenciasAPI{emergencias: &casoFake{casos: []emergencias.CasoProyectado{caso}}}

	pag, err := api.ListarEmergencias(ctxConAudiencia(auth.AudienciaCliente), uuid.New())
	if err != nil {
		t.Fatalf("ListarEmergencias: %v", err)
	}
	if len(pag.Items) != 1 {
		t.Fatalf("se esperaba 1 caso, hubo %d", len(pag.Items))
	}
	got := pag.Items[0]
	if got.Estado != msgclient.EstadoEmergencia("en_atencion") {
		t.Errorf("estado: got %q, want en_atencion", got.Estado)
	}
	if got.ActorReal != nil {
		t.Errorf("cliente NO debe ver actor_real, fue %v", *got.ActorReal)
	}
	if got.Origen != "" {
		t.Errorf("cliente NO debe ver origen, fue %q", got.Origen)
	}
}

// TestListarEmergencias_InternoVeCamposInternos: para audiencia Interno, la
// proyección expone origen y actor_real.
func TestListarEmergencias_InternoVeCamposInternos(t *testing.T) {
	caso := unCaso()
	api := &emergenciasAPI{emergencias: &casoFake{casos: []emergencias.CasoProyectado{caso}}}

	pag, err := api.ListarEmergencias(ctxConAudiencia(auth.AudienciaInterno), uuid.New())
	if err != nil {
		t.Fatalf("ListarEmergencias: %v", err)
	}
	got := pag.Items[0]
	if got.Origen != msgclient.OrigenActivacion("operacion") {
		t.Errorf("interno debe ver origen operacion, fue %q", got.Origen)
	}
	if got.ActorReal == nil {
		t.Errorf("interno debe ver actor_real, fue nil")
	}
}

// TestListarEmergencias_SinClaims: sin claims en el ctx, no autorizado.
func TestListarEmergencias_SinClaims(t *testing.T) {
	api := &emergenciasAPI{emergencias: &casoFake{}}
	_, err := api.ListarEmergencias(context.Background(), uuid.New())
	if err == nil {
		t.Fatalf("se esperaba error de no autorizado sin claims")
	}
}
