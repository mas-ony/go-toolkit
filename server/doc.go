// Package server holds the parts of a Fiber application's wiring that are
// functions rather than configuration: the error handler every service
// installs, and the recover middleware's stack-trace handler. Package
// config builds every fiber.* section from configuration and leaves these
// out, because each closes over a logger that does not exist when
// configuration is read; FiberConfig's documentation says so for
// ErrorHandler, and RecoverConfig.WithStackTraceHandler takes the other.
//
//	fiberCfg := *cfg.Fiber.Config
//	field := cfg.Zerolog.RequestIDField()
//	fiberCfg.ErrorHandler = server.ErrorHandler(log, field)
//	app := fiber.New(fiberCfg)
//	app.Use(fiberrecover.New(cfg.Recover.WithStackTraceHandler(
//		server.StackTraceHandler(log, field))))
//
// # Why not package response
//
// response is the envelope, and it imports nothing: httpclient decodes the
// same Envelope on the client side, and a client has no use for Fiber or
// zerolog. The handlers here render that envelope on the server side, so
// they sit one package out from it, where those imports cost nothing.
//
// # The request id
//
// Every line logged here carries the request's id under requestIDField,
// the key config.ZerologConfig.RequestIDField gives the access log, so a
// failure's line can be joined to the request's access log line and to
// the X-Request-ID header the client received. It reads the id that
// requestid stored, so requestid has to sit outside every route, as
// config's documentation orders the middleware.
package server
