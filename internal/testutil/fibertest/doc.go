// Package fibertest runs a Fiber application for a suite, in the two
// ways the suites here run one.
//
// Send runs one request through the application in process, as app.Test
// does, and hands back the status and the whole body. The config, rest,
// request and server suites send their requests this way, so how a
// request the application could not serve fails a test is written once.
//
// Serve runs the application on a real loopback port, for the suites
// that need what the in-process app.Test cannot show: a connection the
// framework reuses across requests, and a body read off the wire against
// a limit. The request, response and server suites serve an application
// this way, and each has to wait for the same thing before the first
// request: the serving goroutine reaching the point where it accepts. One
// helper is one definition of that wait.
package fibertest
