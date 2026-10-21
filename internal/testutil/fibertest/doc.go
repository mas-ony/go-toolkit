// Package fibertest runs a Fiber application on a real loopback port for
// the suites that need what the in-process app.Test cannot show: a
// connection the framework reuses across requests, and a body read off the
// wire against a limit.
//
// The request and response suites both serve an application this way, and
// both have to wait for the same thing before the first request: the
// serving goroutine reaching the point where it accepts. One helper is one
// definition of that wait.
package fibertest
