// Package config provides the configuration SECTIONS a Fiber service
// needs — app, database, notification, and the fiber.* middleware knobs —
// together with the machinery for assembling them into one validated
// struct.
//
// It deliberately does NOT provide that struct. The root container, the
// loader that builds it, and the rules that span two sections all belong to
// the application, because they are the parts that differ per service. This
// package supplies the pieces; section.go documents the contract they
// satisfy, and MIGRATION.md in the repository root shows a root config.go
// assembled from them.
//
// # Sources and precedence
//
// Two sources, environment first. The application builds one *viper.Viper,
// points it at a config.yaml, and turns on AutomaticEnv, so a variable
// exported in the process environment wins over the same key in the file.
//
// A MISSING FILE IS NOT AN ERROR. A container deployment injects every value
// as a real environment variable and ships no config.yaml at all. A file
// that exists but cannot be parsed IS an error — continuing there would
// start the process with a half-loaded configuration.
//
// # Key spelling
//
// A key is lower_snake_case, dot-nested, and its environment spelling is
// that key uppercased with every dot replaced by an underscore:
//
//	app.name                  APP_NAME
//	database.max_open_conns   DATABASE_MAX_OPEN_CONNS
//	fiber.zerolog.fields      FIBER_ZEROLOG_FIELDS
//
// The replacement is registered with SetEnvKeyReplacer on the Viper the
// application builds. Nothing here derives the environment name a second
// way, so a key renamed in a section file renames its variable with it.
//
// A list-valued key accepts a YAML sequence in the file and a
// comma-separated string in the environment. See splitList in splitlist.go
// for why the comma spelling has to be handled explicitly.
//
// # Sections a deployment does not use
//
// A section no source supplies a single key for is not validated, and the
// startup line records it as not configured rather than printing the zero
// values it is holding. That is what lets this package be used by a service
// that wants three of these sections and not all of them: deleting a block
// from config.yaml is the whole change, with nothing to strip out here.
//
// What it costs is the check that a section is there AT ALL. A block deleted
// by accident and a block a service never had are the same absence. An
// application buys that back by asking for the sections it cannot run
// without, which are exactly the ones it dereferences.
//
// See SuppliedSections for how presence is decided, and for the one way the
// environment half of that decision can be fooled.
//
// # Adding a section
//
// A section is any type with String and Validate, so a service can define
// one locally and put it in its own list without changing this package. Go
// satisfies SectionConfig structurally; there is nothing to register.
//
// Promote a section here once a SECOND service wants it. A section that
// lives here and is used once costs every other consumer a download and
// gives them a block they will never write.
package config
