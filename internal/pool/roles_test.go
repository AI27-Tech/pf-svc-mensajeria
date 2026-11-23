package pool

import (
	"context"
	"strings"
	"testing"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/jackc/pgx/v5/pgconn"
)

type llamada struct {
	sql  string
	args []any
}

type fakeDB struct{ llamadas []llamada }

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.llamadas = append(f.llamadas, llamada{sql, args})
	return pgconn.CommandTag{}, nil
}

func TestConsumidorRolesIdpSubject(t *testing.T) {
	const asignado = `"rol":"cotizacion","asignado_en":"2026-11-20T10:00:00Z"`
	casos := []struct {
		nombre, tipo, payload string
		quiere                int    // llamadas a la base: el hecho y el idp_subject van en una sola
		idp                   string // idp_subject esperado como último argumento
	}{
		{"asignado con idp_subject, una sentencia", TipoRolAsignado, `{"sujeto_id":"s1","idp_subject":"sub1",` + asignado + `}`, 1, "sub1"},
		{"sin idp_subject (evento viejo) lo pasa vacío", TipoRolAsignado, `{"sujeto_id":"s1",` + asignado + `}`, 1, ""},
		{"desactivado con idp_subject", TipoSujetoDesactivado, `{"sujeto_id":"s1","idp_subject":"sub1","desactivado_en":"2026-11-20T10:00:00Z"}`, 1, "sub1"},
		{"reactivado con idp_subject", TipoSujetoReactivado, `{"sujeto_id":"s1","idp_subject":"sub1","reactivado_en":"2026-11-20T10:00:00Z"}`, 1, "sub1"},
		{"revocado con idp_subject", TipoRolRevocado, `{"sujeto_id":"s1","idp_subject":"sub1","rol":"cotizacion","revocado_en":"2026-11-20T10:00:00Z"}`, 1, "sub1"},
		{"hecho inválido no escribe idp_subject", TipoRolAsignado, `{"sujeto_id":"s1","idp_subject":"sub1","rol":"cotizacion"}`, 0, ""},
		{"sin sujeto_id no escribe", TipoRolAsignado, `{"idp_subject":"sub1",` + asignado + `}`, 0, ""},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			db := &fakeDB{}
			err := NuevoConsumidorRoles(db).Procesar(context.Background(), bus.Sobre{Tipo: c.tipo, Payload: []byte(c.payload)})
			if err != nil || len(db.llamadas) != c.quiere {
				t.Fatalf("err=%v, llamadas=%d; quiere %d", err, len(db.llamadas), c.quiere)
			}
			if c.quiere == 1 {
				a := db.llamadas[0].args
				if got := a[len(a)-1]; got != c.idp || !strings.Contains(db.llamadas[0].sql, "idp_subject") {
					t.Errorf("idp_subject = %v, quiere %q: %+v", got, c.idp, db.llamadas[0])
				}
			}
		})
	}
}
