package catalogoscache

import (
	"context"
	"errors"
	"testing"
	"time"

	catclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	"github.com/google/uuid"
)

// subyacenteFake cuenta cuántos ids pidió cada llamada a ListarClientes y permite
// forzar un error (catalogos caído). Devuelve un Cliente por cada id solicitado.
type subyacenteFake struct {
	llamadas  int   // número de viajes a catalogos
	idsPorVez []int // cuántos ids llevó cada viaje
	err       error // si !=nil, ListarClientes falla
}

func (f *subyacenteFake) ListarClientes(ctx context.Context, ids []catclient.Id) (*catclient.PaginaClientes, error) {
	f.llamadas++
	f.idsPorVez = append(f.idsPorVez, len(ids))
	if f.err != nil {
		return nil, f.err
	}
	items := make([]catclient.Cliente, 0, len(ids))
	for _, id := range ids {
		items = append(items, catclient.Cliente{Id: id, Nombre: "cliente-" + id.String()})
	}
	return &catclient.PaginaClientes{Items: items}, nil
}

type relojManual struct{ t time.Time }

func (r *relojManual) ahora() time.Time        { return r.t }
func (r *relojManual) avanzar(d time.Duration) { r.t = r.t.Add(d) }

// TestHitDentroDelTTLNoReconsulta: un lote ya cacheado dentro del TTL se sirve de
// caché sin volver a catalogos.
func TestHitDentroDelTTLNoReconsulta(t *testing.T) {
	sub := &subyacenteFake{}
	rel := &relojManual{t: time.Unix(0, 0)}
	c := NuevaConReloj(sub, time.Minute, rel.ahora)

	ids := []catclient.Id{uuid.New(), uuid.New()}
	if _, err := c.ListarClientes(context.Background(), ids); err != nil {
		t.Fatalf("primera lectura: %v", err)
	}
	rel.avanzar(30 * time.Second)
	if _, err := c.ListarClientes(context.Background(), ids); err != nil {
		t.Fatalf("segunda lectura: %v", err)
	}
	if sub.llamadas != 1 {
		t.Errorf("segunda lectura debía servirse de caché; viajes a catalogos = %d, want 1", sub.llamadas)
	}
}

// TestLoteCombinaHitsYMiss: con parte del lote en caché y parte no, solo se pide
// a catalogos lo que falta, en UN solo viaje (evita N+1).
func TestLoteCombinaHitsYMiss(t *testing.T) {
	sub := &subyacenteFake{}
	rel := &relojManual{t: time.Unix(0, 0)}
	c := NuevaConReloj(sub, time.Minute, rel.ahora)

	a, b, d := uuid.New(), uuid.New(), uuid.New()
	// Cachea a y b.
	if _, err := c.ListarClientes(context.Background(), []catclient.Id{a, b}); err != nil {
		t.Fatalf("precarga: %v", err)
	}
	// Pide a, b (hits) + d (miss): solo d debe viajar.
	pag, err := c.ListarClientes(context.Background(), []catclient.Id{a, b, d})
	if err != nil {
		t.Fatalf("lote combinado: %v", err)
	}
	if sub.llamadas != 2 {
		t.Fatalf("se esperaban 2 viajes (precarga + faltante); hubo %d", sub.llamadas)
	}
	if got := sub.idsPorVez[1]; got != 1 {
		t.Errorf("el segundo viaje debía pedir solo 1 id (el faltante); pidió %d", got)
	}
	if len(pag.Items) != 3 {
		t.Errorf("el lote debe devolver los 3 clientes; devolvió %d", len(pag.Items))
	}
	// Orden preservado: a, b, d.
	if pag.Items[0].Id != a || pag.Items[1].Id != b || pag.Items[2].Id != d {
		t.Errorf("orden del lote no preservado: %+v", []catclient.Id{pag.Items[0].Id, pag.Items[1].Id, pag.Items[2].Id})
	}
}

// TestTTLComoTechoDeObsolescencia: pasado el TTL, el lote vence y se reconsulta
// aunque no haya llegado un evento (broker caído => obsolescencia acotada).
func TestTTLComoTechoDeObsolescencia(t *testing.T) {
	sub := &subyacenteFake{}
	rel := &relojManual{t: time.Unix(0, 0)}
	c := NuevaConReloj(sub, time.Minute, rel.ahora)

	ids := []catclient.Id{uuid.New()}
	if _, err := c.ListarClientes(context.Background(), ids); err != nil {
		t.Fatalf("primera: %v", err)
	}
	rel.avanzar(90 * time.Second)
	if _, err := c.ListarClientes(context.Background(), ids); err != nil {
		t.Fatalf("segunda: %v", err)
	}
	if sub.llamadas != 2 {
		t.Errorf("vencido el TTL debía reconsultar (2); hubo %d", sub.llamadas)
	}
}

// TestCatalogosCaidoSinRespaldoPropaga: si catalogos falla y no hay nada en caché
// que servir, se propaga el error degradado (no se inventa un cliente).
func TestCatalogosCaidoSinRespaldoPropaga(t *testing.T) {
	fallo := errors.New("catalogos caído")
	sub := &subyacenteFake{err: fallo}
	c := Nueva(sub, time.Minute)

	_, err := c.ListarClientes(context.Background(), []catclient.Id{uuid.New()})
	if !errors.Is(err, fallo) {
		t.Fatalf("sin respaldo en caché, el error debe propagarse; got %v", err)
	}
}

// TestCatalogosCaidoSirveObsoleto: si catalogos falla pero hay una entrada vencida
// de respaldo, se sirve esa (obsoleta) en vez de romper (mensajeria.md (f)).
func TestCatalogosCaidoSirveObsoleto(t *testing.T) {
	sub := &subyacenteFake{}
	rel := &relojManual{t: time.Unix(0, 0)}
	c := NuevaConReloj(sub, time.Minute, rel.ahora)

	id := uuid.New()
	if _, err := c.ListarClientes(context.Background(), []catclient.Id{id}); err != nil {
		t.Fatalf("precarga: %v", err)
	}
	// Vence la entrada y catalogos cae: debe servir el respaldo obsoleto.
	rel.avanzar(90 * time.Second)
	sub.err = errors.New("catalogos caído")
	pag, err := c.ListarClientes(context.Background(), []catclient.Id{id})
	if err != nil {
		t.Fatalf("con respaldo obsoleto no debe romper; got %v", err)
	}
	if len(pag.Items) != 1 || pag.Items[0].Id != id {
		t.Errorf("debía servir el cliente obsoleto de respaldo; got %+v", pag.Items)
	}
}

// TestInvalidarForzaReconsulta: tras Invalidar, la siguiente lectura de ese
// cliente va a catalogos aunque el TTL no haya vencido.
func TestInvalidarForzaReconsulta(t *testing.T) {
	sub := &subyacenteFake{}
	rel := &relojManual{t: time.Unix(0, 0)}
	c := NuevaConReloj(sub, time.Hour, rel.ahora)

	id := uuid.New()
	if _, err := c.ListarClientes(context.Background(), []catclient.Id{id}); err != nil {
		t.Fatalf("primera: %v", err)
	}
	c.Invalidar(EntidadCliente, id)
	if _, err := c.ListarClientes(context.Background(), []catclient.Id{id}); err != nil {
		t.Fatalf("segunda: %v", err)
	}
	if sub.llamadas != 2 {
		t.Errorf("tras Invalidar debía reconsultar (2); hubo %d", sub.llamadas)
	}
}

// TestInvalidarIdempotenteYPorEntidad: invalidar dos veces, o invalidar una
// entidad que no es cliente, no explota ni tiene efecto adicional (P13).
func TestInvalidarIdempotenteYPorEntidad(t *testing.T) {
	sub := &subyacenteFake{}
	rel := &relojManual{t: time.Unix(0, 0)}
	c := NuevaConReloj(sub, time.Hour, rel.ahora)

	id := uuid.New()
	if _, err := c.ListarClientes(context.Background(), []catclient.Id{id}); err != nil {
		t.Fatalf("primera: %v", err)
	}
	// Entidad distinta: no-op (no toca la entrada de cliente).
	c.Invalidar("vehiculo", id)
	// Doble invalidación de cliente.
	c.Invalidar(EntidadCliente, id)
	c.Invalidar(EntidadCliente, id)

	if _, err := c.ListarClientes(context.Background(), []catclient.Id{id}); err != nil {
		t.Fatalf("segunda: %v", err)
	}
	if sub.llamadas != 2 {
		t.Errorf("una invalidación efectiva (la de vehiculo es no-op); se esperaban 2 viajes, hubo %d", sub.llamadas)
	}
}
