// Package identidad contiene el adaptador HTTP del puerto mensajeria.Alcances
// (identidad.listarAlcances, DEC-149 #20) con el cliente generado de
// pf-pkg-contracts. Sirve al despliegue en que mensajeria corre aparte de
// identidad (DEC-150 #1); en el compuesto, core pasa la sub-fachada en proceso.
package identidad

import (
	"context"
	"fmt"
	"net/http"

	idclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/identidad"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
)

// AlcancesHTTP implementa mensajeria.Alcances. listarAlcances exige un token de
// servicio válido: el doer que inyecta la raíz lo agrega. El presupuesto de
// tiempo lo pone el llamador (la reconstrucción), igual que en proceso (I3).
type AlcancesHTTP struct {
	cli *idclient.ClientWithResponses
}

var _ mensajeria.Alcances = (*AlcancesHTTP)(nil)

// NewAlcancesHTTP crea el adaptador apuntando a la URL base de identidad.
func NewAlcancesHTTP(baseURL string, doer idclient.HttpRequestDoer) (*AlcancesHTTP, error) {
	opts := []idclient.ClientOption{}
	if doer != nil {
		opts = append(opts, idclient.WithHTTPClient(doer))
	}
	cli, err := idclient.NewClientWithResponses(baseURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("mensajeria: crear cliente de identidad: %w", err)
	}
	return &AlcancesHTTP{cli: cli}, nil
}

// ListarAlcances pide el snapshot (sin cursor) o su continuación (con cursor).
func (a *AlcancesHTTP) ListarAlcances(ctx context.Context, params idclient.ListarAlcancesParams) (*idclient.PaginaAlcances, error) {
	resp, err := a.cli.ListarAlcancesWithResponse(ctx, &params)
	if err != nil {
		return nil, platformerrors.NewDegradadoErr(err, "mensajeria: ListarAlcances contra identidad falló")
	}
	switch resp.StatusCode() {
	case http.StatusOK:
		return resp.JSON200, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, platformerrors.NewNoAutorizado("identidad: no autorizado")
	default:
		return nil, platformerrors.NewDegradado("identidad: respuesta no disponible (status %d)", resp.StatusCode())
	}
}
