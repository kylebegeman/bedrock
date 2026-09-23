package manifest

import (
	"fmt"
	"strings"
)

// Scaffold describes an app to write a starting manifest for.
//
// The rendered file is commented, because the point of a scaffold is to
// teach the schema to whoever reads it next. That rules out marshalling a
// Manifest, which would drop every comment, so the YAML is assembled as
// text and then parsed back through Parse before it is returned. A template
// that cannot produce a valid manifest is a bug here, not a mistake by the
// person who ran the command, and it is reported that way.
type Scaffold struct {
	// App is the app's name, and the default for its route's first label.
	App string
	// Kind is the shape of the first workload: web, static, worker or cron.
	Kind Kind
	// Host is the hostname to route to, for web and static.
	Host string
	// Port is the container port a web workload listens on.
	Port int
	// Schedule is when a cron workload runs.
	Schedule string
	// Postgres adds a database. Every workload of an app with one is given
	// DATABASE_URL, so nothing else has to be declared to use it.
	Postgres bool
	// Dir is the directory a static site is served from. Default public.
	Dir string
	// Existing says the source already holds what the kind runs from, a
	// Dockerfile or the site's files, so Next needn't ask for it.
	Existing bool
}

// DefaultPort is what a web workload is assumed to listen on.
const DefaultPort = 8000

// DefaultSchedule runs a cron workload nightly, at an hour when nothing
// else is happening.
const DefaultSchedule = "0 3 * * *"

// Check reports whether the scaffold can be rendered, naming the flag to
// fix rather than the field.
func (s Scaffold) Check() error {
	if !namePattern.MatchString(s.App) {
		return fmt.Errorf("the app name %q must be lowercase letters, digits and hyphens, up to 40 characters", s.App)
	}
	if reservedApps[s.App] {
		return fmt.Errorf("%q is a name bedrock uses for itself; pick another", s.App)
	}
	switch s.Kind {
	case Web, Static:
		if s.Host == "" {
			return fmt.Errorf("a %s app is reached by a hostname: pass --host, such as --host %s.example.com", s.Kind, s.App)
		}
		if !hostPattern.MatchString(s.Host) {
			return fmt.Errorf("--host %q isn't a hostname such as %s.example.com", s.Host, s.App)
		}
	case Worker, Cron:
		if s.Host != "" {
			return fmt.Errorf("a %s workload serves no traffic, so it takes no --host", s.Kind)
		}
	case "":
		return fmt.Errorf("--kind is required: web, static, worker or cron")
	default:
		return fmt.Errorf("--kind %q isn't one of web, static, worker, cron", s.Kind)
	}
	if s.Kind == Web && s.Port != 0 && (s.Port < 1 || s.Port > 65535) {
		return fmt.Errorf("--port %d isn't a port number", s.Port)
	}
	if s.Kind != Web && s.Port != 0 {
		return fmt.Errorf("--port is for a web workload; a %s workload listens on nothing", s.Kind)
	}
	if s.Kind != Cron && s.Schedule != "" {
		return fmt.Errorf("--schedule is for a cron workload")
	}
	if s.Dir != "" && (s.Kind != Static || strings.HasPrefix(s.Dir, "/") || strings.Contains(s.Dir, "..")) {
		return fmt.Errorf("--dir %q must be a directory inside a static site's source", s.Dir)
	}
	if s.Schedule != "" {
		if _, err := ParseSchedule(s.Schedule); err != nil {
			return fmt.Errorf("--schedule: %w", err)
		}
	}
	return nil
}

// Render writes the manifest. The result is guaranteed to parse and
// validate; see the note on Scaffold.
func (s Scaffold) Render() ([]byte, error) {
	if err := s.Check(); err != nil {
		return nil, err
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("# %s, as bedrock deploys it.\n", s.App)
	w("#\n")
	w("# Anything left out takes its default, and the defaults are the safe\n")
	w("# ones: a read-only root, no capabilities, a non-root user. Run\n")
	w("# `bedrock deploy` from the directory holding this file.\n")
	w("app: %s\n", s.App)
	w("description: What this app is\n")
	w("owner: personal\n")
	w("\n")
	w("workloads:\n")

	switch s.Kind {
	case Static:
		dir := s.Dir
		if dir == "" {
			dir = "public"
		}
		w("  # A static workload serves files straight from the source. It has\n")
		w("  # no image and no port; the edge serves the directory itself.\n")
		w("  pages:\n")
		w("    kind: static\n")
		w("    dir: %s\n", dir)
		w("    routes:\n")
		w("      - host: %s\n", s.Host)
	case Web:
		port := s.Port
		if port == 0 {
			port = DefaultPort
		}
		w("  web:\n")
		w("    kind: web\n")
		w("    # Built from ./Dockerfile. To deploy an image that is already\n")
		w("    # published, drop `build` and give `image: registry/name:tag`.\n")
		w("    build:\n")
		w("      context: .\n")
		w("    # The port the process listens on inside the container.\n")
		w("    port: %d\n", port)
		w("    routes:\n")
		w("      - host: %s\n", s.Host)
		w("    # Until this answers 200 the new revision is never given traffic,\n")
		w("    # and a deploy that cannot pass it rolls back on its own.\n")
		w("    health:\n")
		w("      path: /healthz\n")
		w("    resources:\n")
		w("      memory: 512m\n")
	case Worker:
		w("  # A worker runs without listening, so it has no port and no routes.\n")
		w("  worker:\n")
		w("    kind: worker\n")
		w("    build:\n")
		w("      context: .\n")
		w("    # Without a command the image's own CMD runs.\n")
		w("    # command: [./app, worker]\n")
		w("    #\n")
		w("    # singleton keeps the old copy from overlapping the new one\n")
		w("    # during a deploy, for work that must not run twice at once.\n")
		w("    # singleton: true\n")
		w("    resources:\n")
		w("      memory: 512m\n")
	case Cron:
		schedule := s.Schedule
		if schedule == "" {
			schedule = DefaultSchedule
		}
		w("  # A cron workload runs its command on a schedule and exits. It is\n")
		w("  # never given traffic, so it has no port and no routes.\n")
		w("  job:\n")
		w("    kind: cron\n")
		w("    build:\n")
		w("      context: .\n")
		w("    schedule: %q\n", schedule)
		w("    # A run that outlives its timeout is stopped and recorded failed.\n")
		w("    timeout: 30m\n")
		w("    # command: [./app, nightly]\n")
		w("    resources:\n")
		w("      memory: 512m\n")
	}

	if s.Postgres {
		w("\n")
		w("  # A release workload runs once per deploy, after the database is up\n")
		w("  # and before the new revision starts. This is where migrations go.\n")
		w("  # It is commented out because only you know the command; a release\n")
		w("  # step that runs the wrong thing runs it on every single deploy.\n")
		w("  #\n")
		w("  # migrate:\n")
		w("  #   kind: release\n")
		w("  #   order: 1\n")
		w("  #   build:\n")
		w("  #     context: .\n")
		w("  #   command: [./app, migrate]\n")
		w("  #   timeout: 5m\n")
	}

	if s.Postgres {
		w("\n")
		w("data:\n")
		w("  # Every workload above is given DATABASE_URL pointing here, so\n")
		w("  # nothing else needs declaring to use it. bedrock generates the\n")
		w("  # password, seals it, and never writes it to a log or a receipt.\n")
		w("  postgres:\n")
		w("    version: \"17\"\n")
		w("\n")
		w("backup:\n")
		w("  # An app with data is backed up nightly by default, with a weekly\n")
		w("  # drill that restores it somewhere safe to prove it opens.\n")
		w("  #\n")
		w("  # A verify query makes a backup prove it holds real rows before it\n")
		w("  # counts. Point it at a table that is never legitimately empty.\n")
		w("  #\n")
		w("  # verify:\n")
		w("  #   sql: select count(*) from users\n")
		w("  #   at_least: 1\n")
	}

	if s.Host != "" {
		w("\n")
		w("# Checked after the deploy. A failure here rolls the deploy back.\n")
		w("checks:\n")
		w("  - url: https://%s/\n", s.Host)
	}

	out := []byte(b.String())
	if _, err := Parse(out); err != nil {
		return nil, fmt.Errorf("the generated manifest is not valid, which is a bug in bedrock: %w", err)
	}
	return out, nil
}

// Next says what the person still has to supply, in the order they will
// need it. An empty result means the app is ready to deploy as written.
func (s Scaffold) Next() []string {
	var next []string
	switch {
	case s.Existing:
	case s.Kind == Static:
		dir := s.Dir
		if dir == "" {
			dir = "public"
		}
		next = append(next, "put the site's files in ./"+dir)
	default:
		next = append(next, "write a ./Dockerfile that builds the app")
	}
	if s.Kind == Web {
		next = append(next, fmt.Sprintf("serve /healthz, or drop the health block from %s", FileName))
	}
	if s.Postgres {
		next = append(next, "read DATABASE_URL from the environment; bedrock sets it")
	}
	if s.Host != "" {
		next = append(next, fmt.Sprintf("point %s at this machine, or let bedrock write the DNS record", s.Host))
	}
	return next
}
