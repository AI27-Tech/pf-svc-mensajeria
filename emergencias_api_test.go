package mensajeria

import (
	"context"
	"errors"
	"testing"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/autorizacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/emergencias"
	msgtipos "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
	"github.com/google/uuid"
)

// casoFake es un casoDeEmergencia doble: solo implementa ListarPorViaje con
// casos fijos; el resto no se invoca en estas pruebas.
type casoFake struct {
	casos []emergencias.CasoProyectado
}

func (f *casoFake) ListarPorViaje(context.Context, string) ([]emergencias.CasoProyectado, error) {
	return f.casos, nil
}
func (f *casoFake) Bandeja(context.Context) ([]emergencias.CasoProyectado, error) {
	panic("no se invoca")
}
func (f *casoFake) Obtener(context.Context, string, string) (emergencias.CasoProyectado, error) {
	panic("no se invoca")
}
func (f *casoFake) ListarNotas(context.Context, string, string) ([]emergencias.NotaProyectada, error) {
	panic("no se invoca")
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
func (f *casoFake) Liberar(context.Context, string, string, string, string, msgtipos.VersionReclamo, bool) error {
	panic("no se invoca")
}
func (f *casoFake) Transferir(context.Context, string, string, string, string, msgtipos.VersionReclamo, bool) error {
	panic("no se invoca")
}
func (f *casoFake) Resolver(context.Context, string, string, string, msgtipos.VersionReclamo) error {
	panic("no se invoca")
}
func (f *casoFake) Cerrar(context.Context, string, string, string) error { panic("no se invoca") }

// clienteDelViaje es el cliente de todo viaje en estas pruebas.
var clienteDelViaje = uuid.MustParse("c0000000-0000-0000-0000-0000000000c0")

// autzFake responde el cliente del viaje y del caso; el resto no se invoca.
type autzFake struct {
	cliente string
	err     error
}

func (f autzFake) ClienteDelViaje(context.Context, string) (string, error) { return f.cliente, f.err }
func (f autzFake) ClienteDeEmergencia(context.Context, string) (string, error) {
	return f.cliente, f.err
}
func (autzFake) TenantDeEmergencia(context.Context, string, *auth.Claims) (string, error) {
	panic("no se invoca")
}
func (autzFake) Emergencia(context.Context, string, *auth.Claims, autorizacion.Permiso) (string, error) {
	panic("no se invoca")
}

// alcanceFake es la proyección: los clientes de cualquier sujeto, o un error.
type alcanceFake struct {
	clientes []uuid.UUID
	err      error
	leido    *bool
}

func (f alcanceFake) Clientes(context.Context, string) ([]uuid.UUID, error) {
	if f.leido != nil {
		*f.leido = true
	}
	return f.clientes, f.err
}

// enAlcance arma la fachada con el viaje dentro del alcance de todo sujeto.
func enAlcance(c *casoFake) *emergenciasAPI {
	return &emergenciasAPI{emergencias: c, autz: autzFake{cliente: clienteDelViaje.String()},
		alcance: alcanceFake{clientes: []uuid.UUID{clienteDelViaje}}}
}

func ctxConAudiencia(aud auth.Audiencia) context.Context {
	rol := "cliente"
	if aud == auth.AudienciaInterno {
		rol = "monitorista"
	}
	return ctxConRol(aud, rol)
}

func ctxConRol(aud auth.Audiencia, rol string) context.Context {
	return auth.ContextWithClaims(context.Background(), &auth.Claims{
		Subject:   "11111111-1111-1111-1111-111111111111",
		TenantID:  "t-1",
		Rol:       rol,
		Audiencia: aud,
	})
}

// TestEmergencias_RolesDelMonitoreo: listar por viaje lo hacen monitoreo y
// Administrador, igual que activar (DEC-135, DEC-152). Los demás roles
// internos reciben 403 antes de tocar el dominio (el doble entra en pánico si se
// llamara a Activar).
func TestEmergencias_RolesDelMonitoreo(t *testing.T) {
	api := enAlcance(&casoFake{casos: []emergencias.CasoProyectado{unCaso()}})
	lista := map[string]bool{"monitorista": true, "supervisor": true, "coordinador": true, "administrador": true,
		"reaccion": false, "ejecutivo": false, "jefe_operativo": false}
	for rol, puede := range lista {
		_, err := api.ListarEmergencias(ctxConRol(auth.AudienciaInterno, rol), uuid.New())
		if puede && err != nil {
			t.Errorf("listar como %s: %v, quiere éxito", rol, err)
		}
		if !puede && platformerrors.AsCategory(err) != platformerrors.CategoryProhibido {
			t.Errorf("listar como %s: err %v, quiere prohibido", rol, err)
		}
	}
	for _, rol := range []string{"reaccion", "ejecutivo", "jefe_operativo"} {
		_, _, err := api.ActivarEmergencia(ctxConRol(auth.AudienciaInterno, rol), uuid.New(),
			msgclient.ActivarEmergenciaEntrada{ActivacionId: uuid.New()})
		if platformerrors.AsCategory(err) != platformerrors.CategoryProhibido {
			t.Errorf("activar como %s: err %v, quiere prohibido", rol, err)
		}
	}
}

// unCaso es un caso interno con todos los campos poblados.
func unCaso() emergencias.CasoProyectado {
	return emergencias.CasoProyectado{
		ID:             uuid.New().String(),
		TenantID:       "t-1",
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
	api := enAlcance(&casoFake{casos: []emergencias.CasoProyectado{caso}})

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

// TestListarEmergencias_ClienteSoloSuEmpresaYNoCerrados: el dominio devuelve los
// casos del viaje de todos los tenants (DEC-129); al Cliente solo le quedan los
// de su empresa que no están cerrados (ADR 21 §6). El interno los ve todos.
func TestListarEmergencias_ClienteSoloSuEmpresaYNoCerrados(t *testing.T) {
	propio, ajeno, cerrado := unCaso(), unCaso(), unCaso()
	ajeno.TenantID = "t-otro"
	cerrado.Estado = "cerrada"
	api := enAlcance(&casoFake{casos: []emergencias.CasoProyectado{propio, ajeno, cerrado}})

	pag, err := api.ListarEmergencias(ctxConAudiencia(auth.AudienciaCliente), uuid.New())
	if err != nil {
		t.Fatalf("ListarEmergencias: %v", err)
	}
	if len(pag.Items) != 1 || pag.Items[0].Id.String() != propio.ID {
		t.Fatalf("cliente: got %+v, want solo el caso %s", pag.Items, propio.ID)
	}
	pag, err = api.ListarEmergencias(ctxConAudiencia(auth.AudienciaInterno), uuid.New())
	if err != nil {
		t.Fatalf("ListarEmergencias interno: %v", err)
	}
	if len(pag.Items) != 3 {
		t.Fatalf("interno: got %d casos, want 3", len(pag.Items))
	}
}

// TestListarEmergencias_InternoVeCamposInternos: para audiencia Interno, la
// proyección expone origen y actor_real.
func TestListarEmergencias_InternoVeCamposInternos(t *testing.T) {
	caso := unCaso()
	api := enAlcance(&casoFake{casos: []emergencias.CasoProyectado{caso}})

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
	api := enAlcance(&casoFake{})
	_, err := api.ListarEmergencias(context.Background(), uuid.New())
	if err == nil {
		t.Fatalf("se esperaba error de no autorizado sin claims")
	}
}

// TestListarEmergencias_SujetoNoUUID: un sujeto que no es UUID no identifica a un
// actor; se rechaza como no autorizado antes del caso de uso (que aquí devolvería
// éxito), igual que la ausencia de claims.
func TestListarEmergencias_SujetoNoUUID(t *testing.T) {
	api := enAlcance(&casoFake{})
	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{
		Subject: "u-1", TenantID: "t-1", Audiencia: auth.AudienciaInterno,
	})
	_, err := api.ListarEmergencias(ctx, uuid.New())
	if got := platformerrors.AsCategory(err); got != platformerrors.CategoryNoAutorizado {
		t.Fatalf("categoría: got %v (%v), want no_autorizado", got, err)
	}
}

// TestExigirAlcance fija la guarda de DEC-149 #6 sobre listarEmergencias, que
// comparte con activar, anotar y cerrar: el monitoreo fuera de sus clientes, sin
// proyección (sin fila) o sobre un viaje que mensajeria no conoce recibe 404; un
// error de la proyección se propaga, nunca como "todos"; el administrador y el
// Cliente no leen la proyección.
func TestExigirAlcance(t *testing.T) {
	otro := uuid.New()
	casos := []struct {
		nombre string
		rol    string
		autz   autzFake
		proy   alcanceFake
		quiere platformerrors.Category
		lee    bool
	}{
		{"monitorista dentro", "monitorista", autzFake{cliente: clienteDelViaje.String()}, alcanceFake{clientes: []uuid.UUID{otro, clienteDelViaje}}, platformerrors.CategoryUnknown, true},
		{"supervisor fuera", "supervisor", autzFake{cliente: clienteDelViaje.String()}, alcanceFake{clientes: []uuid.UUID{otro}}, platformerrors.CategoryNoEncontrado, true},
		{"coordinador sin proyección", "coordinador", autzFake{cliente: clienteDelViaje.String()}, alcanceFake{}, platformerrors.CategoryNoEncontrado, true},
		{"monitorista, viaje desconocido", "monitorista", autzFake{cliente: ""}, alcanceFake{clientes: []uuid.UUID{clienteDelViaje}}, platformerrors.CategoryNoEncontrado, true},
		{"monitorista, proyección caída", "monitorista", autzFake{cliente: clienteDelViaje.String()}, alcanceFake{err: errors.New("db")}, platformerrors.CategoryRoto, true},
		{"monitorista, lectura del viaje caída", "monitorista", autzFake{err: platformerrors.NewRoto("db")}, alcanceFake{}, platformerrors.CategoryRoto, false},
		{"administrador sin alcance", "administrador", autzFake{cliente: clienteDelViaje.String()}, alcanceFake{}, platformerrors.CategoryUnknown, false},
	}
	for _, c := range casos {
		var leido bool
		c.proy.leido = &leido
		api := &emergenciasAPI{emergencias: &casoFake{casos: []emergencias.CasoProyectado{unCaso()}}, autz: c.autz, alcance: c.proy}
		_, err := api.ListarEmergencias(ctxConRol(auth.AudienciaInterno, c.rol), uuid.New())
		if got := platformerrors.AsCategory(err); got != c.quiere {
			t.Errorf("%s: categoría %v (%v); quiere %v", c.nombre, got, err, c.quiere)
		}
		if leido != c.lee {
			t.Errorf("%s: leyó la proyección = %v; quiere %v", c.nombre, leido, c.lee)
		}
	}
	// El Cliente no pasa por el alcance de monitoreo: lo acota su empresa.
	var leido bool
	api := &emergenciasAPI{emergencias: &casoFake{}, autz: autzFake{}, alcance: alcanceFake{leido: &leido}}
	if _, err := api.ListarEmergencias(ctxConAudiencia(auth.AudienciaCliente), uuid.New()); err != nil || leido {
		t.Errorf("cliente: err %v, leyó la proyección %v; quiere éxito sin leerla", err, leido)
	}
}
