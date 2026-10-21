package mensajeria

import (
	"context"
	"time"

	catclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
)

// Puertos de salida que declara mensajeria COMO CONSUMIDOR (regla 2 de 00, ADR
// 17). La interfaz la declara el consumidor (aquí, público) para que la raíz
// —cmd/api standalone o cmd/mensajeria— la implemente con el adaptador HTTP de
// adapters/http/. mensajeria vive en su propio deployable y catalogos vive en
// core: no existe adaptador en proceso para esta arista, siempre es HTTP (00,
// tabla de servicios; mensajeria.md (f)). Los tipos de entrada/salida son los
// generados de pf-pkg-contracts, nunca escritos a mano (regla 6).

// PresupuestoCatalogos es el presupuesto de tiempo de la arista a catalogos. Se
// exporta JUNTO AL PUERTO porque la obligación I3 (ADR 17) exige que el
// adaptador HTTP use un valor único y estable. 2 s es el default común en los
// servicios hermanos (reportes), no un número de negocio aprobado.
const PresupuestoCatalogos = 2 * time.Second

// CatalogosLector es la arista síncrona hacia catalogos para los datos de
// cliente/contrato/contacto que mensajeria cachea en proceso, invalidada por
// catalogo.actualizado (mensajeria.md (f)). Debe leer POR LOTE, nunca por
// elemento (evita N+1): resolver una bandeja toca muchos clientes a la vez.
//
// Solo se declaran operaciones que EXISTEN en catalogos.yaml de la versión
// fijada (regla 6). En tracer.7, catalogos expone la lectura por lote de
// clientes (`listarClientes?ids=`), que es lo que esta interfaz usa.
//
// BRECHA C-cat (no se corrige aquí; contrato primero): mensajeria.md (f) pide
// además "datos de contacto" por lote, que catalogos.yaml NO expone todavía
// (no hay operación ni schema de contacto). El puerto no inventa ese método;
// la caché de contacto queda bloqueada hasta que el contrato lo exponga, y el
// adaptador en adapters/http/catalogos/ solo implementa el lote de clientes.
type CatalogosLector interface {
	// ListarClientes lee un lote de clientes por sus identificadores (lectura
	// por lote; `listarClientes?ids=`). Nunca por elemento.
	ListarClientes(ctx context.Context, ids []catclient.Id) (*catclient.PaginaClientes, error)
}

// RevocacionPort es la arista RESTRINGIDA hacia identidad (DEC-051): solo dos
// operaciones, para el snapshot al arrancar (ADR 26 §4) y el fallback en modo
// degradado (ADR 26 §5). mensajeria verifica tokens como todo módulo protegido;
// no es una dependencia de dominio sino de infraestructura de autorización, y
// SIEMPRE es HTTP (00, tabla de servicios). El adaptador HTTP vive en
// adapters/http/; no existe adaptador en proceso porque mensajeria no co-ubica
// con identidad.
//
// La difusión interna de sesion.revocada a todas las réplicas (ADR 26 §3) es un
// mecanismo APARTE, modelado en internal/revocacion y alimentado por la
// suscripción durable del módulo, no por este puerto.
type RevocacionPort interface {
	// ListarRevocadas devuelve las sesiones revocadas para el snapshot inicial.
	ListarRevocadas(ctx context.Context) ([]string, error)
	// ObtenerSesion consulta una sesión puntual tras 15 s sin frescura.
	ObtenerSesion(ctx context.Context, sesionID string) (revocada bool, err error)
}

// PresupuestoRevocacion es el presupuesto de tiempo de UNA llamada puntual de la
// arista restringida a identidad (snapshot o fallback), no el umbral de frescura
// del canal (15 s / corte 30 s, que fija ADR 26). 2 s es el default común, igual
// que PresupuestoCatalogos; no es un número de negocio aprobado.
const PresupuestoRevocacion = 2 * time.Second
