// Package catalogoscache es la caché EN PROCESO del lado consumidor que
// mensajeria mantiene sobre la arista síncrona a catalogos (mensajeria.md (f),
// E5-T5). No es una caché que administre catalogos: catalogos solo emite
// catalogo.actualizado (identidad + versión, nunca el payload); cada consumidor
// cachea por su cuenta y la invalida con ese evento. Esta es la mitad caché; la
// otra —el consumidor durable que llama Invalidar— vive en internal/consumidores.
//
// La caché decora el puerto CatalogosLector: implementa la MISMA interfaz
// (ListarClientes por lote) envolviendo al adaptador HTTP. Así el caso de uso no
// sabe que hay caché; solo ve el puerto.
//
// A diferencia de operacion (que cachea aptitud por un id único), mensajeria lee
// POR LOTE (ListarClientes([ids])). La caché indexa CADA cliente por su id, de
// modo que un catalogo.actualizado de un `cliente_id` suelto invalida solo esa
// entrada. Una lectura por lote se resuelve combinando los aciertos vivos de
// caché con UN solo viaje a catalogos por los que falten o estén vencidos (nunca
// uno por elemento: evita N+1).
//
// Dos límites a la obsolescencia (criterios de E5-T5):
//   - TTL corto: una entrada vence por tiempo aunque nunca llegue un evento. Con
//     el broker caído, la obsolescencia queda ACOTADA por el TTL.
//   - Invalidación por evento: catalogo.actualizado borra la entrada de ese
//     cliente; la próxima lectura va a catalogos y recachea.
//
// Degradación (mensajeria.md (f): "sirve con caché/sin la dimensión, no se
// rompe"): si catalogos no responde para los faltantes, se devuelve lo que la
// caché tenga —incluso vencido— y solo se propaga el error degradado cuando NO
// hay nada que servir. La caché nunca convierte el fallo en un dato inventado.
package catalogoscache

import (
	"context"
	"sync"
	"time"

	catclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
)

// EntidadCliente es la única entidad de catálogo que mensajeria cachea en V1.
// Coincide con el campo `entidad` del evento catalogo.actualizado. Contrato y
// contacto quedan fuera (brecha C-cat: catalogos.yaml no los expone todavía), así
// que un evento de otra entidad no tiene entrada aquí y su invalidación es no-op.
const EntidadCliente = "cliente"

// reloj es la fuente de tiempo, inyectable para pruebas deterministas del TTL.
type reloj func() time.Time

// entrada es un cliente cacheado con el instante en que se guardó, para medir el
// TTL contra el reloj.
type entrada struct {
	cli       catclient.Cliente
	guardadaA time.Time
}

// CatalogosLector es la MISMA forma del puerto mensajeria.CatalogosLector, pero
// declarada aquí para no importar el paquete raíz (evitaría un ciclo: la raíz
// importa este paquete para cablear la caché en New). Satisfacer esta interfaz
// satisface la de la raíz por estructura.
type CatalogosLector interface {
	ListarClientes(ctx context.Context, ids []catclient.Id) (*catclient.PaginaClientes, error)
}

// CacheClientes decora CatalogosLector con una caché en proceso de TTL corto,
// invalidable por cliente_id. Es segura para uso concurrente.
type CacheClientes struct {
	sub CatalogosLector
	ttl time.Duration
	now reloj

	mu    sync.Mutex
	datos map[catclient.Id]entrada
}

// Garantía en compilación: la caché ES un CatalogosLector (decorador transparente).
var _ CatalogosLector = (*CacheClientes)(nil)

// ttlPorDefecto es el techo de obsolescencia cuando no se configura uno. Corto a
// propósito: acota la ventana en que un cliente puede quedar obsoleto si el
// broker está caído y no llega catalogo.actualizado. No es un número de negocio
// aprobado; si debe configurarse por despliegue, se pasa por el constructor.
const ttlPorDefecto = 30 * time.Second

// Nueva construye la caché sobre un puerto subyacente, con un TTL corto como
// techo de obsolescencia. ttl<=0 toma el default. Reloj time.Now; usa
// NuevaConReloj en pruebas.
func Nueva(sub CatalogosLector, ttl time.Duration) *CacheClientes {
	return NuevaConReloj(sub, ttl, time.Now)
}

// NuevaConReloj es como Nueva pero con reloj inyectable (pruebas del TTL).
func NuevaConReloj(sub CatalogosLector, ttl time.Duration, now reloj) *CacheClientes {
	if ttl <= 0 {
		ttl = ttlPorDefecto
	}
	if now == nil {
		now = time.Now
	}
	return &CacheClientes{
		sub:   sub,
		ttl:   ttl,
		now:   now,
		datos: make(map[catclient.Id]entrada),
	}
}

// ListarClientes resuelve el lote combinando aciertos vivos de caché con UN solo
// viaje a catalogos por los que falten o estén vencidos. El resultado conserva el
// orden de `ids` pedido. Si catalogos falla al resolver los faltantes, se
// devuelve lo que la caché tenga (incluso vencido) y solo se propaga el error
// cuando no hay NADA que servir (degradación de mensajeria.md (f)).
func (c *CacheClientes) ListarClientes(ctx context.Context, ids []catclient.Id) (*catclient.PaginaClientes, error) {
	vivos := make(map[catclient.Id]catclient.Cliente, len(ids))
	obsoletos := make(map[catclient.Id]catclient.Cliente, len(ids))
	var faltantes []catclient.Id

	ahora := c.now()
	c.mu.Lock()
	for _, id := range ids {
		e, ok := c.datos[id]
		switch {
		case ok && ahora.Sub(e.guardadaA) < c.ttl:
			vivos[id] = e.cli
		case ok:
			// Vencido: candidato a refrescar, pero se conserva como respaldo por si
			// catalogos no responde (degradación: servir obsoleto antes que romper).
			obsoletos[id] = e.cli
			faltantes = append(faltantes, id)
		default:
			faltantes = append(faltantes, id)
		}
	}
	c.mu.Unlock()

	// Un solo viaje a catalogos por los faltantes (nunca por elemento).
	frescos := make(map[catclient.Id]catclient.Cliente, len(faltantes))
	if len(faltantes) > 0 {
		pag, err := c.sub.ListarClientes(ctx, faltantes)
		if err != nil {
			// catalogos caído: no se puede refrescar. Si no hay nada vivo ni obsoleto
			// que servir, se propaga el degradado; si hay respaldo, se sirve obsoleto.
			if len(vivos) == 0 && len(obsoletos) == 0 {
				return nil, err
			}
		} else if pag != nil {
			nuevaMarca := c.now()
			c.mu.Lock()
			for _, cli := range pag.Items {
				c.datos[cli.Id] = entrada{cli: cli, guardadaA: nuevaMarca}
				frescos[cli.Id] = cli
			}
			c.mu.Unlock()
		}
	}

	// Recomponer en el orden pedido: fresco > vivo > obsoleto (respaldo). Un id que
	// catalogos no devolvió (p. ej. no existe) simplemente no aparece, igual que
	// haría catalogos directo.
	items := make([]catclient.Cliente, 0, len(ids))
	for _, id := range ids {
		if cli, ok := frescos[id]; ok {
			items = append(items, cli)
		} else if cli, ok := vivos[id]; ok {
			items = append(items, cli)
		} else if cli, ok := obsoletos[id]; ok {
			items = append(items, cli)
		}
	}
	return &catclient.PaginaClientes{Items: items}, nil
}

// Invalidar borra de la caché la entrada de un cliente ante catalogo.actualizado.
// La invoca el consumidor. Es idempotente: invalidar algo ausente no hace nada
// —reentregar el evento dos veces solo invalida una vez, sin efecto extra
// (P13)—. Una entidad que esta caché no indexa (contrato, sla, vehiculo, …) no
// tiene entrada y el barrido es un no-op.
func (c *CacheClientes) Invalidar(entidad string, id catclient.Id) {
	if entidad != EntidadCliente {
		return // mensajeria solo cachea clientes en V1
	}
	c.mu.Lock()
	delete(c.datos, id)
	c.mu.Unlock()
}
