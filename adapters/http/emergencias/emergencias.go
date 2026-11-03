// Package emergencias es el adaptador HTTP entrante de la familia de emergencia:
// las rutas por viaje (contrato tracer.17, /api/v1/viajes/{id}/emergencias...) y
// la bandeja de Reacción y las acciones por id del caso (tracer.34,
// /api/v1/bandeja/emergencias y /api/v1/emergencias/{eid}...). El bff antepone
// auth.Middleware, así que los claims llegan en el contexto.
//
// Esta capa solo decodifica el contrato, llama a la superficie en proceso
// (mensajeria.EmergenciasAPI, la única copia de las reglas) y codifica la
// respuesta. No autoriza ni traduce errores de dominio: lo hace la superficie.
package emergencias

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"

	"github.com/AI27-Tech/pf-svc-mensajeria/adapters/http/boundary"
)

// API es la superficie de emergencia que estos handlers sirven. La satisface
// mensajeria.EmergenciasAPI; se declara aquí (consumidor) porque el paquete raíz
// importa este y no puede ser importado de vuelta.
type API interface {
	ListarEmergencias(ctx context.Context, viajeID msgclient.Id) (*msgclient.PaginaEmergencias, error)
	ActivarEmergencia(ctx context.Context, viajeID msgclient.Id, body msgclient.ActivarEmergenciaEntrada) (*msgclient.CasoEmergencia, bool, error)
	AgregarNotaEmergencia(ctx context.Context, viajeID, emergenciaID msgclient.Id, body msgclient.NotaEmergenciaEntrada) error
	ListarBandejaEmergencias(ctx context.Context, params msgclient.ListarBandejaEmergenciasParams) (*msgclient.PaginaBandejaEmergencias, error)
	ObtenerEmergencia(ctx context.Context, emergenciaID msgclient.Id) (*msgclient.CasoEmergencia, error)
	ListarNotasEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id, params msgclient.ListarNotasEmergenciaPorIdParams) (*msgclient.PaginaNotasEmergencia, error)
	ReclamarEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id, body msgclient.VersionEsperadaEntrada) (*msgclient.Reclamo, error)
	LiberarReclamoEmergenciaPorId(ctx context.Context, emergenciaID, reclamoID msgclient.Id, params msgclient.LiberarReclamoEmergenciaPorIdParams) error
	TransferirEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id, body msgclient.TransferenciaEntrada) (*msgclient.Reclamo, error)
	ResolverEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id, body msgclient.ResolucionEmergenciaEntrada) (*msgclient.EmergenciaReaccion, error)
	CerrarEmergenciaPorId(ctx context.Context, emergenciaID msgclient.Id) (*msgclient.CasoEmergencia, error)
}

// Handlers agrupa las dependencias de los handlers de emergencia.
type Handlers struct {
	API API
}

// Montar registra las rutas de emergencia en el mux. Cada acción por viaje y su
// gemela por id comparten handler: solo cambia de dónde sale {eid}.
func (h Handlers) Montar(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/viajes/{id}/emergencias", h.listar)
	mux.HandleFunc("POST /api/v1/viajes/{id}/emergencias", h.activar)
	mux.HandleFunc("POST /api/v1/viajes/{id}/emergencias/{eid}/notas", h.agregarNota)

	mux.HandleFunc("GET /api/v1/bandeja/emergencias", h.bandeja)
	mux.HandleFunc("GET /api/v1/emergencias/{eid}", h.obtener)
	mux.HandleFunc("GET /api/v1/emergencias/{eid}/notas", h.listarNotas)

	for _, base := range []string{"/api/v1/viajes/{id}/emergencias/{eid}", "/api/v1/emergencias/{eid}"} {
		mux.HandleFunc("POST "+base+"/reclamos", h.reclamar)
		mux.HandleFunc("DELETE "+base+"/reclamos/{rid}", h.liberar)
		mux.HandleFunc("POST "+base+"/transferencias", h.transferir)
	}
	// resolver y cerrar responden distinto por familia: 200 sin cuerpo por viaje
	// (como hasta tracer.33), 200 con el caso por id (tracer.34).
	mux.HandleFunc("POST /api/v1/viajes/{id}/emergencias/{eid}/resolucion", h.resolver(false))
	mux.HandleFunc("POST /api/v1/emergencias/{eid}/resolucion", h.resolver(true))
	mux.HandleFunc("POST /api/v1/viajes/{id}/emergencias/{eid}/cierre", h.cerrar(false))
	mux.HandleFunc("POST /api/v1/emergencias/{eid}/cierre", h.cerrar(true))
}

func (h Handlers) listar(w http.ResponseWriter, r *http.Request) {
	viaje, ok := idRuta(w, r, "id")
	if !ok {
		return
	}
	pagina, err := h.API.ListarEmergencias(r.Context(), viaje)
	responder(w, http.StatusOK, pagina, err)
}

// activar responde 201 con el caso nuevo, o 200 con el original si es una
// reentrega de la misma activacion_id.
func (h Handlers) activar(w http.ResponseWriter, r *http.Request) {
	viaje, ok := idRuta(w, r, "id")
	if !ok {
		return
	}
	var entrada msgclient.ActivarEmergenciaEntrada
	if !decodificar(w, r, &entrada) {
		return
	}
	caso, idempotente, err := h.API.ActivarEmergencia(r.Context(), viaje, entrada)
	estado := http.StatusCreated
	if idempotente {
		estado = http.StatusOK
	}
	responder(w, estado, caso, err)
}

func (h Handlers) agregarNota(w http.ResponseWriter, r *http.Request) {
	viaje, ok := idRuta(w, r, "id")
	if !ok {
		return
	}
	eid, ok := idRuta(w, r, "eid")
	if !ok {
		return
	}
	var entrada msgclient.NotaEmergenciaEntrada
	if !decodificar(w, r, &entrada) {
		return
	}
	responder[struct{}](w, http.StatusCreated, nil, h.API.AgregarNotaEmergencia(r.Context(), viaje, eid, entrada))
}

func (h Handlers) bandeja(w http.ResponseWriter, r *http.Request) {
	pagina, err := h.API.ListarBandejaEmergencias(r.Context(), msgclient.ListarBandejaEmergenciasParams{})
	responder(w, http.StatusOK, pagina, err)
}

func (h Handlers) obtener(w http.ResponseWriter, r *http.Request) {
	eid, ok := idRuta(w, r, "eid")
	if !ok {
		return
	}
	caso, err := h.API.ObtenerEmergencia(r.Context(), eid)
	responder(w, http.StatusOK, caso, err)
}

func (h Handlers) listarNotas(w http.ResponseWriter, r *http.Request) {
	eid, ok := idRuta(w, r, "eid")
	if !ok {
		return
	}
	pagina, err := h.API.ListarNotasEmergenciaPorId(r.Context(), eid, msgclient.ListarNotasEmergenciaPorIdParams{})
	responder(w, http.StatusOK, pagina, err)
}

func (h Handlers) reclamar(w http.ResponseWriter, r *http.Request) {
	eid, ok := idRuta(w, r, "eid")
	if !ok {
		return
	}
	var entrada msgclient.VersionEsperadaEntrada
	if !decodificar(w, r, &entrada) {
		return
	}
	reclamo, err := h.API.ReclamarEmergenciaPorId(r.Context(), eid, entrada)
	responder(w, http.StatusCreated, reclamo, err)
}

// liberar exige version_esperada en el query.
func (h Handlers) liberar(w http.ResponseWriter, r *http.Request) {
	eid, ok := idRuta(w, r, "eid")
	if !ok {
		return
	}
	rid, ok := idRuta(w, r, "rid")
	if !ok {
		return
	}
	ver, err := strconv.ParseInt(r.URL.Query().Get("version_esperada"), 10, 64)
	if err != nil {
		boundary.WriteError(w, platformerrors.NewValidacion("version_esperada inválida o faltante"))
		return
	}
	err = h.API.LiberarReclamoEmergenciaPorId(r.Context(), eid, rid,
		msgclient.LiberarReclamoEmergenciaPorIdParams{VersionEsperada: ver})
	responder[struct{}](w, http.StatusNoContent, nil, err)
}

func (h Handlers) transferir(w http.ResponseWriter, r *http.Request) {
	eid, ok := idRuta(w, r, "eid")
	if !ok {
		return
	}
	var entrada msgclient.TransferenciaEntrada
	if !decodificar(w, r, &entrada) {
		return
	}
	reclamo, err := h.API.TransferirEmergenciaPorId(r.Context(), eid, entrada)
	responder(w, http.StatusCreated, reclamo, err)
}

func (h Handlers) resolver(conCuerpo bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		eid, ok := idRuta(w, r, "eid")
		if !ok {
			return
		}
		var entrada msgclient.ResolucionEmergenciaEntrada
		if !decodificar(w, r, &entrada) {
			return
		}
		caso, err := h.API.ResolverEmergenciaPorId(r.Context(), eid, entrada)
		if !conCuerpo {
			caso = nil
		}
		responder(w, http.StatusOK, caso, err)
	}
}

func (h Handlers) cerrar(conCuerpo bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		eid, ok := idRuta(w, r, "eid")
		if !ok {
			return
		}
		caso, err := h.API.CerrarEmergenciaPorId(r.Context(), eid)
		if !conCuerpo {
			caso = nil
		}
		responder(w, http.StatusOK, caso, err)
	}
}

// idRuta lee un parámetro de ruta UUID. Un valor que no es UUID no nombra ningún
// recurso: 404, como un recurso inexistente.
func idRuta(w http.ResponseWriter, r *http.Request, nombre string) (msgclient.Id, bool) {
	id, err := uuid.Parse(r.PathValue(nombre))
	if err != nil {
		boundary.WriteError(w, platformerrors.NewNoEncontrado("recurso no encontrado"))
		return uuid.Nil, false
	}
	return id, true
}

func decodificar(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		boundary.WriteError(w, platformerrors.NewValidacion("cuerpo inválido"))
		return false
	}
	return true
}

// responder escribe el error (sin su causa interna, boundary.WriteError) o el
// estado con el cuerpo; un cuerpo nil no se escribe.
func responder[T any](w http.ResponseWriter, status int, cuerpo *T, err error) {
	if err != nil {
		boundary.WriteError(w, err)
		return
	}
	if cuerpo == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(cuerpo)
}
