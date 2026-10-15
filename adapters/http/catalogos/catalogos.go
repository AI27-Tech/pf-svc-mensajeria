// Package catalogos contiene el adaptador HTTP del puerto CatalogosLector de
// mensajeria (cliente generado de pf-pkg-contracts). mensajeria y catalogos
// viven en deployables distintos, así que esta arista es SIEMPRE HTTP: no existe
// adaptador en proceso (00, tabla de servicios; mensajeria.md (f)). Lo usa
// cmd/api y cmd/mensajeria por igual.
package catalogos

import (
	"context"
	"fmt"
	"net/http"

	catclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
)

// CatalogosHTTP implementa mensajeria.CatalogosLector con el cliente generado de
// catalogos. Respeta el presupuesto de tiempo (mensajeria.PresupuestoCatalogos)
// y traduce los errores a la taxonomía de la plataforma (I3).
type CatalogosHTTP struct {
	cli *catclient.ClientWithResponses
}

var _ mensajeria.CatalogosLector = (*CatalogosHTTP)(nil)

// NewCatalogosHTTP crea el adaptador apuntando a la URL base de catalogos.
func NewCatalogosHTTP(baseURL string, doer catclient.HttpRequestDoer) (*CatalogosHTTP, error) {
	opts := []catclient.ClientOption{}
	if doer != nil {
		opts = append(opts, catclient.WithHTTPClient(doer))
	}
	cli, err := catclient.NewClientWithResponses(baseURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("mensajeria: crear cliente de catalogos: %w", err)
	}
	return &CatalogosHTTP{cli: cli}, nil
}

// ListarClientes lee un lote de clientes por sus identificadores
// (`listarClientes?ids=`). Nunca por elemento (evita N+1).
func (a *CatalogosHTTP) ListarClientes(ctx context.Context, ids []catclient.Id) (*catclient.PaginaClientes, error) {
	ctx, cancel := context.WithTimeout(ctx, mensajeria.PresupuestoCatalogos)
	defer cancel()

	resp, err := a.cli.ListarClientesWithResponse(ctx, &catclient.ListarClientesParams{Ids: &ids})
	if err != nil {
		return nil, traducirError("ListarClientes", err)
	}
	if resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
		return nil, traducirStatus("ListarClientes", resp.StatusCode())
	}
	return resp.JSON200, nil
}

// traducirStatus mapea el código HTTP a la taxonomía de la plataforma. La
// indisponibilidad de catalogos es degradado: mensajeria sirve con caché/sin la
// dimensión, no se rompe.
func traducirStatus(op string, status int) error {
	switch status {
	case http.StatusNotFound:
		return platformerrors.NewNoEncontrado("catalogos: %s no encontrado", op)
	case http.StatusUnauthorized, http.StatusForbidden:
		return platformerrors.NewNoAutorizado("catalogos: %s no autorizado", op)
	default:
		return platformerrors.NewDegradado("catalogos: %s no disponible (status %d)", op, status)
	}
}

func traducirError(op string, err error) error {
	return platformerrors.NewDegradadoErr(err, "mensajeria: %s contra catalogos falló", op)
}
