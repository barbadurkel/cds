package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-gorp/gorp"
	"github.com/gorilla/mux"
	"github.com/rockbears/log"

	"github.com/ovh/cds/engine/api/event"
	"github.com/ovh/cds/engine/api/services"
	"github.com/ovh/cds/engine/api/worker"
	"github.com/ovh/cds/engine/gorpmapper"
	"github.com/ovh/cds/engine/service"
	"github.com/ovh/cds/sdk"
)

func (api *API) getServiceHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		typeService := vars["type"]

		if !isCDN(ctx) {
			return sdk.WrapError(sdk.ErrForbidden, "only CDN can call this route")
		}

		var srvs []sdk.Service
		var err error
		if typeService == sdk.TypeHatchery {
			srvs, err = services.LoadAllByType(ctx, api.mustDB(), typeService)
			if err != nil {
				return err
			}
		}

		var servicesConf []sdk.ServiceConfiguration
		for _, s := range srvs {
			servicesConf = append(servicesConf, sdk.ServiceConfiguration{
				URL:       s.HTTPURL,
				Name:      s.Name,
				ID:        s.ID,
				PublicKey: base64.StdEncoding.EncodeToString(s.PublicKey),
				Type:      s.Type,
			})
		}

		return service.WriteJSON(w, servicesConf, http.StatusOK)
	}
}

// This has to be called by the signin handler
func (api *API) serviceRegister(ctx context.Context, tx gorpmapper.SqlExecutorWithTx, data *sdk.Service) error {
	consumer := getAPIConsumer(ctx)
	data.LastHeartbeat = time.Now()
	if data.Name == "" {
		return sdk.NewErrorFrom(sdk.ErrWrongRequest, "missing service name")
	}

	if consumer.Type != sdk.ConsumerBuiltin {
		return sdk.WrapError(sdk.ErrForbidden, "cannot register service from a consumer that is not of type \"builtin\"")
	}
	if isWorker(ctx) {
		return sdk.WrapError(sdk.ErrForbidden, "cannot register a service from a consumer that is associated with a worker")
	}

	// Service that are not hatcheries should be started as an admin
	if data.Type != sdk.TypeHatchery && !isAdmin(ctx) {
		return sdk.WrapError(sdk.ErrForbidden, "cannot register service of type %s for consumer %s", data.Type, consumer.ID)
	}

	// Try to find the service, and keep; else generate a new one
	srv, err := services.LoadByConsumerID(ctx, tx, consumer.ID)
	if err != nil && !sdk.ErrorIs(err, sdk.ErrNotFound) {
		return err
	}
	exists := srv != nil

	if exists && srv.Type != data.Type {
		return sdk.WrapError(sdk.ErrForbidden, "cannot register service %s of type %s for consumer %s while existing service type is different", data.Name, data.Type, consumer.ID)
	}

	// Update or create the service
	session := getAuthSession(ctx)
	if session == nil {
		return sdk.NewErrorFrom(sdk.ErrUnauthorized, "missing registered session")
	}
	if exists {
		srv.Update(*data)
		if err := services.Update(ctx, tx, srv); err != nil {
			return err
		}
		log.Debug(ctx, "postServiceRegisterHandler> update existing service %s(%d) registered for consumer %s", srv.Name, srv.ID, *srv.ConsumerID)
	} else {
		srv = data
		srv.ConsumerID = &consumer.ID

		if err := services.Insert(ctx, tx, srv); err != nil {
			return sdk.WithStack(err)
		}
		log.Debug(ctx, "postServiceRegisterHandler> insert new service %s(%d) registered for consumer %s", srv.Name, srv.ID, *srv.ConsumerID)
	}

	var strRegion string
	if srv.Region != nil {
		strRegion = fmt.Sprintf(" from region %q", *srv.Region)
	}
	log.Info(ctx, "Registering service %s(%d) %s, consumer: %s, session %s", srv.Name, srv.ID, strRegion, consumer.ID, session.ID)

	if err := services.UpsertStatus(ctx, tx, *srv, session.ID); err != nil {
		return sdk.WithStack(err)
	}

	if len(srv.PublicKey) > 0 {
		log.Debug(ctx, "postServiceRegisterHandler> service %s registered with public key: %s", srv.Name, string(srv.PublicKey))
	}

	// For hatchery service we need to check if there are workers that are not attached to an existing hatchery
	// If some worker's parent consumer match current hatchery consumer we will attach this worker to the new hatchery.
	if srv.Type == sdk.TypeHatchery {
		if err := worker.ReAttachAllToHatchery(ctx, tx, *srv); err != nil {
			return err
		}
	}

	srv.Uptodate = data.Version == sdk.VERSION
	*data = *srv
	return nil
}

func (api *API) postServiceHearbeatHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		if ok := isService(ctx); !ok {
			return sdk.WithStack(sdk.ErrForbidden)
		}

		s, err := services.LoadByID(ctx, api.mustDB(), getAPIConsumer(ctx).Service.ID)
		if err != nil {
			return err
		}

		var mon sdk.MonitoringStatus
		if err := service.UnmarshalBody(r, &mon); err != nil {
			return err
		}

		// Update status to warn if service version != api version
		for i := range mon.Lines {
			if mon.Lines[i].Component == "Version" {
				if sdk.VERSION != mon.Lines[i].Value {
					mon.Lines[i].Status = sdk.MonitoringStatusWarn
				} else {
					mon.Lines[i].Status = sdk.MonitoringStatusOK
				}
				break
			}
		}

		tx, err := api.mustDB().Begin()
		if err != nil {
			return sdk.WithStack(err)
		}
		defer tx.Rollback() // nolint

		s.LastHeartbeat = time.Now()
		s.MonitoringStatus = mon

		var sessionID string
		if a := getAuthSession(ctx); a != nil {
			sessionID = a.ID
		}
		if err := services.UpdateLastHeartbeat(ctx, tx, s); err != nil {
			return err
		}

		if err := services.UpsertStatus(ctx, tx, *s, sessionID); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return sdk.WithStack(err)
		}

		return nil
	}
}

func (api *API) serviceAPIHeartbeat(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second).C

	// first call
	api.serviceAPIHeartbeatUpdate(ctx, api.mustDB())

	for {
		select {
		case <-ctx.Done():
			if ctx.Err() != nil {
				log.Error(ctx, "Exiting serviceAPIHeartbeat: %v", ctx.Err())
				return
			}
		case <-tick:
			api.serviceAPIHeartbeatUpdate(ctx, api.mustDB())
		}
	}
}

func (api *API) serviceAPIHeartbeatUpdate(ctx context.Context, db *gorp.DbMap) {
	tx, err := db.Begin()
	if err != nil {
		log.Error(ctx, "serviceAPIHeartbeat> error on repo.Begin:%v", err)
		return
	}
	defer tx.Rollback() // nolint

	var srvConfig sdk.ServiceConfig
	b, _ := json.Marshal(api.Config)
	sdk.JSONUnmarshal(b, &srvConfig) // nolint

	srv := &sdk.Service{
		CanonicalService: sdk.CanonicalService{
			Name:   event.GetCDSName(),
			Type:   sdk.TypeAPI,
			Config: srvConfig,
		},
		MonitoringStatus: *api.Status(ctx),
		LastHeartbeat:    time.Now(),
	}

	old, err := services.LoadByName(ctx, tx, srv.Name)
	if err != nil && !sdk.ErrorIs(err, sdk.ErrNotFound) {
		log.Error(ctx, "serviceAPIHeartbeat> Unable to find service by name: %v", err)
		return
	}
	exists := old != nil

	if exists && old.ConsumerID != nil {
		log.Error(ctx, "serviceAPIHeartbeat> Can't save an api service as one service already exists for given name %s", srv.Name)
		return
	}

	var authSessionID string
	if a := getAuthSession(ctx); a != nil {
		authSessionID = a.ID
	}
	if exists {
		srv.ID = old.ID
		if err := services.Update(ctx, tx, srv); err != nil {
			log.Error(ctx, "serviceAPIHeartbeat> Unable to update service %s: %v", srv.Name, err)
			return
		}
	} else {
		if err := services.Insert(ctx, tx, srv); err != nil {
			log.Error(ctx, "serviceAPIHeartbeat> Unable to insert service %s: %v", srv.Name, err)
			return
		}
	}

	if err := services.UpsertStatus(ctx, tx, *srv, authSessionID); err != nil {
		log.Error(ctx, "serviceAPIHeartbeat> Unable to insert or update monitoring status %s: %v", srv.Name, err)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Error(ctx, "serviceAPIHeartbeat> error tx commit: %v", err)
		return
	}
}
