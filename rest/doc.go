// Package rest is the HTTP side of package crud: the endpoints of a table
// whose service is a crud.Service, and the pieces an endpoint of a
// handler's own is written with.
//
// Table serves the two endpoints every id-keyed table has, GET on the
// collection and DELETE /:id. CRUD adds POST on the collection, GET /:id
// and PUT /:id over a crud.Service. A handler embeds the one that fits
// and adds endpoints, or declares one of the same name, which shadows the
// promoted one. Either way a handler keeps only what is its own: the
// request body, how that body becomes a row, the list filters and any
// access rule.
//
//	type UnitHandler struct {
//		*rest.CRUD[Unit, *Unit, UnitReadOption]
//	}
//
//	func NewUnitHandler(log zerolog.Logger, svc UnitService) *UnitHandler {
//		h := &UnitHandler{}
//		h.CRUD = rest.NewCRUD(rest.NewBase(log, "unit", "unit"),
//			svc, h.filter, h.decode)
//		return h
//	}
//
// Base, List, BatchRows, CreateBatch and RequireOwner are what an
// endpoint of a handler's own is written with, so it answers as the
// promoted ones do.
//
// # Errors are returned, never written
//
// Every failure is answered by returning a *fiber.Error, which
// server.ErrorHandler renders in response's envelope, as every success
// is rendered. A helper that checks something returns such an error for
// its caller to return in turn. One that wrote the response itself would
// get nil back from c.JSON, and its caller would carry on and overwrite
// the response it meant to stop at; package request keeps the same rule
// for the same reason.
//
// # What is logged, and where
//
// A 500 carries only a message naming what failed, "failed to <action>
// <entity>". The error behind it is logged here, once, at error level,
// with the request's method and path, and never sent to the client: this
// is the HTTP layer that the crud documentation says logs a failure where
// it answers 500. A body that does not decode and a refused caller are
// logged at warn level, the refusal with the caller's id when
// Base.Caller is set. A 404 is not logged.
//
// sql.ErrNoRows from a service, a write that matched no row, answers 404
// with the handler's Missing message rather than 500.
//
// # Writes are guarded before the body is read
//
// Table.Guard, when set, runs before PUT and DELETE touch the row, and
// before PUT decodes its body, so a caller the guard refuses learns
// nothing from how the body would have been checked. RequireOwner builds
// the usual guard. A handler that serves a write of its own calls Check
// with the row id, which applies the same rule.
//
// # What the tests hold in place
//
// The unit suite drives every endpoint through a real Fiber router, with
// server.ErrorHandler installed, over a fake service, so each status and
// message asserted is what a client receives: the 201, 200 and 404
// paths, the 400s of Bind and Required, the 500 that keeps its cause in
// the log, RequireOwner's 403 and 404 and its log line, the order of
// guard and decode, and the envelope checks of BatchRows. Each source
// file's tests sit in the test file named for it, over the fake service
// and the app base_test.go declares.
package rest
