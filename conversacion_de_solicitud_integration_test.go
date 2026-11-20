package mensajeria_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	platformpgx "github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/google/uuid"
)

// TestConversacionDeSolicitud ejerce, contra Postgres real y por las dos vías
// (en proceso y HTTP), lo que el contrato tracer.45 dice de la conversación de la
// solicitud: se resuelve por el id de la solicitud, la audiencia depende del tipo,
// y las bandejas de chat y de solicitudes no se mezclan aunque la solicitud y el
// viaje compartan viaje_id.
func TestConversacionDeSolicitud(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		cliente      = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		otroCliente  = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		compartido   = "11111111-1111-1111-1111-111111111111" // solicitud y viaje: mismo id
		soloViaje    = "22222222-2222-2222-2222-222222222222"
		soloSolicitu = "33333333-3333-3333-3333-333333333333"
	)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := conversacion.NewServicio(pool, outbox.NewPublisher())
	for _, e := range []struct {
		id        string
		solicitud bool
		viaje     bool
	}{{compartido, true, true}, {soloViaje, false, true}, {soloSolicitu, true, false}} {
		in := conversacion.EntradaCreacion{ViajeID: e.id, ClienteID: cliente}
		if e.solicitud {
			if _, err := svc.CrearSolicitudPorEvento(ctx, in); err != nil {
				t.Fatal(err)
			}
		}
		if e.viaje {
			if _, err := svc.CrearPorEvento(ctx, in); err != nil {
				t.Fatal(err)
			}
		}
	}

	chat := &platformpgx.Pool{Pool: pool}
	mod, err := mensajeria.New(mensajeria.Pools{Chat: chat, Despacho: chat}, mensajeria.Deps{})
	if err != nil {
		t.Fatalf("construir módulo: %v", err)
	}
	api := mod.Conversaciones()
	mux := http.NewServeMux()
	mod.Routes(mux)

	como := func(rol string, aud auth.Audiencia, tenant string) context.Context {
		return auth.ContextWithClaims(ctx, &auth.Claims{Subject: uuid.NewString(), TenantID: tenant, Rol: rol, Audiencia: aud})
	}
	interno := func(rol string) context.Context { return como(rol, auth.AudienciaInterno, "ai27") }
	ctxCliente := como("cliente", auth.AudienciaCliente, cliente)
	ctxOtro := como("cliente", auth.AudienciaCliente, otroCliente)
	categoria := platformerrors.AsCategory
	get := func(c context.Context, ruta string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ruta, nil).WithContext(c))
		return rec.Code
	}
	idDe := func(c context.Context, solicitud string) string {
		t.Helper()
		conv, err := api.ObtenerConversacionDeSolicitud(c, uuid.MustParse(solicitud))
		if err != nil {
			t.Fatalf("ObtenerConversacionDeSolicitud(%s): %v", solicitud, err)
		}
		return conv.Id.String()
	}

	// Quién abre la conversación de la solicitud, por tipo (contrato tracer.45).
	t.Run("audiencia de obtenerConversacionDeSolicitud", func(t *testing.T) {
		for _, c := range []struct {
			nombre string
			ctx    context.Context
		}{{"cliente dueño", ctxCliente}, {"cotizacion", interno("cotizacion")}, {"administrador", interno("administrador")}} {
			conv, err := api.ObtenerConversacionDeSolicitud(c.ctx, uuid.MustParse(compartido))
			if err != nil {
				t.Fatalf("%s: %v", c.nombre, err)
			}
			if conv.Tipo == nil || *conv.Tipo != "solicitud" || conv.ViajeId.String() != compartido {
				t.Errorf("%s: tipo/viaje_id = %v/%s", c.nombre, conv.Tipo, conv.ViajeId)
			}
			if st := get(c.ctx, "/api/v1/solicitudes/"+compartido+"/conversacion"); st != http.StatusOK {
				t.Errorf("%s por HTTP: %d, quiere 200", c.nombre, st)
			}
		}
		for _, rol := range []string{"ejecutivo", "supervisor", "coordinador", "jefe_operativo", "monitorista", "reaccion"} {
			if _, err := api.ObtenerConversacionDeSolicitud(interno(rol), uuid.MustParse(compartido)); categoria(err) != platformerrors.CategoryProhibido {
				t.Errorf("%s: %v, quiere prohibido", rol, err)
			}
			if st := get(interno(rol), "/api/v1/solicitudes/"+compartido+"/conversacion"); st != http.StatusForbidden {
				t.Errorf("%s por HTTP: %d, quiere 403", rol, st)
			}
		}
		if _, err := api.ObtenerConversacionDeSolicitud(ctxOtro, uuid.MustParse(compartido)); categoria(err) != platformerrors.CategoryNoEncontrado {
			t.Errorf("cliente de otra empresa: %v, quiere no_encontrado", err)
		}
		// Un viaje sin solicitud no tiene conversación de solicitud.
		if _, err := api.ObtenerConversacionDeSolicitud(interno("cotizacion"), uuid.MustParse(soloViaje)); categoria(err) != platformerrors.CategoryNoEncontrado {
			t.Errorf("viaje sin solicitud: %v, quiere no_encontrado", err)
		}
	})

	// El viaje y la solicitud con el mismo id no se mezclan en la lectura por id.
	t.Run("obtenerConversacionDeViaje solo devuelve tipo viaje", func(t *testing.T) {
		conv, err := api.ObtenerConversacionDeViaje(interno("ejecutivo"), uuid.MustParse(compartido))
		if err != nil {
			t.Fatal(err)
		}
		if conv.Tipo == nil || *conv.Tipo != "viaje" || conv.Id.String() == idDe(interno("cotizacion"), compartido) {
			t.Fatalf("devolvió %v / %s; quiere la del viaje, distinta de la de la solicitud", conv.Tipo, conv.Id)
		}
		// Una solicitud sin viaje no tiene conversación de viaje.
		if _, err := api.ObtenerConversacionDeViaje(interno("ejecutivo"), uuid.MustParse(soloSolicitu)); categoria(err) != platformerrors.CategoryNoEncontrado {
			t.Fatalf("solicitud sin viaje: %v, quiere no_encontrado", err)
		}
	})

	// La conversación de la solicitud no se lee ni se escribe por las operaciones
	// por {id} con un rol sin acceso (la audiencia se decide por el tipo).
	t.Run("el ejecutivo y supervisor no leen la solicitud por id", func(t *testing.T) {
		id := uuid.MustParse(idDe(interno("cotizacion"), compartido))
		for _, rol := range []string{"ejecutivo", "supervisor", "coordinador"} {
			if _, err := api.ObtenerConversacion(interno(rol), id); categoria(err) != platformerrors.CategoryProhibido {
				t.Errorf("%s obtener: %v, quiere prohibido", rol, err)
			}
			if _, err := api.ListarMensajes(interno(rol), id, msgclient.ListarMensajesParams{}); categoria(err) != platformerrors.CategoryProhibido {
				t.Errorf("%s mensajes: %v, quiere prohibido", rol, err)
			}
		}
		// El cliente de OTRA empresa no se entera de que existe: 404, no 403.
		if _, err := api.ObtenerConversacion(ctxOtro, id); categoria(err) != platformerrors.CategoryNoEncontrado {
			t.Errorf("otro cliente obtener: %v, quiere no_encontrado", err)
		}
		if _, err := api.ListarMensajes(ctxOtro, id, msgclient.ListarMensajesParams{}); categoria(err) != platformerrors.CategoryNoEncontrado {
			t.Errorf("otro cliente mensajes: %v, quiere no_encontrado", err)
		}
		if st := get(ctxOtro, "/api/v1/conversaciones/"+id.String()); st != http.StatusNotFound {
			t.Errorf("otro cliente por HTTP: %d, quiere 404", st)
		}
		if _, err := api.ObtenerConversacion(interno("cotizacion"), id); err != nil {
			t.Errorf("cotizacion obtener: %v", err)
		}
		if _, err := api.ObtenerConversacion(ctxCliente, id); err != nil {
			t.Errorf("cliente obtener: %v", err)
		}
	})

	// Al revés: la audiencia de la solicitud no se extiende a la conversación del viaje.
	t.Run("cotizacion y administrador no leen el viaje por id", func(t *testing.T) {
		viaje, err := api.ObtenerConversacionDeViaje(interno("ejecutivo"), uuid.MustParse(compartido))
		if err != nil {
			t.Fatal(err)
		}
		id := viaje.Id
		for _, rol := range []string{"cotizacion", "administrador"} {
			if _, err := api.ObtenerConversacion(interno(rol), id); categoria(err) != platformerrors.CategoryProhibido {
				t.Errorf("%s obtener: %v, quiere prohibido", rol, err)
			}
			if st := get(interno(rol), "/api/v1/conversaciones/"+id.String()); st != http.StatusForbidden {
				t.Errorf("%s por HTTP: %d, quiere 403", rol, st)
			}
		}
	})

	t.Run("las bandejas de chat solo traen tipo viaje", func(t *testing.T) {
		for _, rol := range []string{"ejecutivo", "supervisor", "coordinador"} {
			tb, err := api.ListarTrabajoBandeja(interno(rol), msgclient.ListarTrabajoMiBandejaParams{})
			if err != nil {
				t.Fatalf("%s: %v", rol, err)
			}
			if len(tb.Items) != 2 {
				t.Fatalf("%s: %d conversaciones, quiere 2 (las de tipo viaje)", rol, len(tb.Items))
			}
			for _, it := range tb.Items {
				if it.Tipo == nil || *it.Tipo != "viaje" || it.ViajeId.String() == soloSolicitu {
					t.Errorf("%s: apareció %v / %s", rol, it.Tipo, it.ViajeId)
				}
			}
		}
		// Catch-up desde 0: el mismo filtro.
		desde := int64(0)
		tb, err := api.ListarTrabajoBandeja(interno("ejecutivo"), msgclient.ListarTrabajoMiBandejaParams{DesdeRevision: &desde})
		if err != nil || len(tb.Items) != 2 {
			t.Fatalf("catch-up: %v, %d items", err, len(tb.Items))
		}
	})

	t.Run("la bandeja de solicitudes solo trae tipo solicitud", func(t *testing.T) {
		for _, rol := range []string{"cotizacion", "administrador"} {
			tb, err := api.ListarTrabajoMiBandejaSolicitudes(interno(rol), msgclient.ListarTrabajoMiBandejaSolicitudesParams{})
			if err != nil {
				t.Fatalf("%s: %v", rol, err)
			}
			if len(tb.Items) != 2 || tb.Revision == 0 {
				t.Fatalf("%s: %d conversaciones, revisión %d; quiere 2 y revisión > 0", rol, len(tb.Items), tb.Revision)
			}
			for _, it := range tb.Items {
				if it.Tipo == nil || *it.Tipo != "solicitud" || it.ViajeId.String() == soloViaje {
					t.Errorf("%s: apareció %v / %s", rol, it.Tipo, it.ViajeId)
				}
			}
			if st := get(interno(rol), "/api/v1/bandeja/solicitudes"); st != http.StatusOK {
				t.Errorf("%s por HTTP: %d, quiere 200", rol, st)
			}
		}
		// Catch-up desde la revisión vigente: nada cambió después.
		tb, _ := api.ListarTrabajoMiBandejaSolicitudes(interno("cotizacion"), msgclient.ListarTrabajoMiBandejaSolicitudesParams{})
		rev := tb.Revision
		tb2, err := api.ListarTrabajoMiBandejaSolicitudes(interno("cotizacion"), msgclient.ListarTrabajoMiBandejaSolicitudesParams{DesdeRevision: &rev})
		if err != nil || len(tb2.Items) != 0 {
			t.Fatalf("catch-up desde %d: %v, %d items; quiere 0", rev, err, len(tb2.Items))
		}
		for _, rol := range []string{"ejecutivo", "supervisor", "coordinador", "jefe_operativo", "monitorista", "reaccion"} {
			if _, err := api.ListarTrabajoMiBandejaSolicitudes(interno(rol), msgclient.ListarTrabajoMiBandejaSolicitudesParams{}); categoria(err) != platformerrors.CategoryNoEncontrado {
				t.Errorf("%s: %v, quiere no_encontrado", rol, err)
			}
			if st := get(interno(rol), "/api/v1/bandeja/solicitudes"); st != http.StatusNotFound {
				t.Errorf("%s por HTTP: %d, quiere 404", rol, st)
			}
		}
		if _, err := api.ListarTrabajoMiBandejaSolicitudes(ctxCliente, msgclient.ListarTrabajoMiBandejaSolicitudesParams{}); categoria(err) != platformerrors.CategoryNoEncontrado {
			t.Errorf("cliente: %v, quiere no_encontrado", err)
		}
	})
}
