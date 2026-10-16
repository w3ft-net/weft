package main

import (
	"fmt"
	"strings"
)

// Targets are positional everywhere: a source is `ns/svc`, a namespace is
// `ns`. The old --namespace/--service/--ns/-n/--name flags are gone, and
// scanArgs rejects every flag a command does not declare, because ignoring one
// would silently widen what the command acts on (an unfiltered `pending`).

// parseTarget splits "ns/svc". The service may itself contain a slash
// ("weft/shard/main"), so only the first one separates.
func parseTarget(arg string) (namespace, service string, err error) {
	ns, svc, ok := strings.Cut(arg, "/")
	if !ok || ns == "" || svc == "" {
		return "", "", fmt.Errorf("%q is not a source: write it as ns/svc", arg)
	}
	return ns, svc, nil
}

// removedFlags are the pre-positional spellings, answered with a pointer to
// the new form instead of a bare "unknown flag".
var removedFlags = map[string]bool{"--namespace": true, "--service": true, "--ns": true, "-n": true, "--name": true}

// scanArgs splits args into positionals and declared flags. values maps a
// value-taking flag (and its aliases) to a canonical name; bools does the same
// for switches. Anything else starting with "-" is an error.
func scanArgs(args []string, values, bools map[string]string) (pos []string, vals map[string]string, on map[string]bool, err error) {
	vals, on = map[string]string{}, map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case values[a] != "":
			if i+1 >= len(args) {
				return nil, nil, nil, fmt.Errorf("%s needs a value", a)
			}
			vals[values[a]] = args[i+1]
			i++
		case bools[a] != "":
			on[bools[a]] = true
		case removedFlags[a]:
			return nil, nil, nil, fmt.Errorf("%s is gone: pass the target as a positional argument (ns/svc for a source, ns for a namespace)", a)
		case strings.HasPrefix(a, "-"):
			return nil, nil, nil, fmt.Errorf("unknown flag %s", a)
		default:
			pos = append(pos, a)
		}
	}
	return pos, vals, on, nil
}

type sourceAddArgs struct{ namespace, service, logGroup, region string }

// parseSourceAddArgs: ns/svc --log-group GROUP [--region REGION].
func parseSourceAddArgs(args []string) (sourceAddArgs, error) {
	pos, vals, _, err := scanArgs(args,
		map[string]string{"--log-group": "log-group", "--region": "region"}, nil)
	if err != nil {
		return sourceAddArgs{}, err
	}
	if len(pos) != 1 || vals["log-group"] == "" {
		return sourceAddArgs{}, fmt.Errorf("usage: weft source add ns/svc --log-group GROUP [--region REGION]")
	}
	ns, svc, err := parseTarget(pos[0])
	if err != nil {
		return sourceAddArgs{}, err
	}
	region := vals["region"]
	if region == "" {
		region = "us-east-2"
	}
	return sourceAddArgs{ns, svc, vals["log-group"], region}, nil
}

// parseSourceRemoveArgs: ns/svc.
func parseSourceRemoveArgs(args []string) (namespace, service string, err error) {
	pos, _, _, err := scanArgs(args, nil, nil)
	if err != nil {
		return "", "", err
	}
	if len(pos) != 1 {
		return "", "", fmt.Errorf("usage: weft source remove ns/svc")
	}
	return parseTarget(pos[0])
}

type sourcePendingArgs struct {
	namespace string
	watch     bool
}

// parseSourcePendingArgs: [NS] [--watch].
func parseSourcePendingArgs(args []string) (sourcePendingArgs, error) {
	pos, _, on, err := scanArgs(args, nil, map[string]string{"--watch": "watch", "-w": "watch"})
	if err != nil {
		return sourcePendingArgs{}, err
	}
	if len(pos) > 1 {
		return sourcePendingArgs{}, fmt.Errorf("usage: weft source pending [NS] [--watch]")
	}
	p := sourcePendingArgs{watch: on["watch"]}
	if len(pos) == 1 {
		p.namespace = pos[0]
	}
	return p, nil
}

type tokenCreateArgs struct{ namespace, label, env string }

// parseTokenCreateArgs: NS [--label LABEL] [--env ENV].
func parseTokenCreateArgs(args []string) (tokenCreateArgs, error) {
	pos, vals, _, err := scanArgs(args, map[string]string{"--label": "label", "--env": "env"}, nil)
	if err != nil {
		return tokenCreateArgs{}, err
	}
	if len(pos) != 1 || strings.Contains(pos[0], "/") {
		return tokenCreateArgs{}, fmt.Errorf("usage: weft token create NS [--label LABEL] [--env ENV]")
	}
	return tokenCreateArgs{pos[0], vals["label"], vals["env"]}, nil
}

type namespaceRemoveArgs struct {
	namespace string
	force     bool
}

// parseNamespaceRemoveArgs: NS [--force].
func parseNamespaceRemoveArgs(args []string) (namespaceRemoveArgs, error) {
	pos, _, on, err := scanArgs(args, nil, map[string]string{"--force": "force"})
	if err != nil {
		return namespaceRemoveArgs{}, err
	}
	if len(pos) != 1 || strings.Contains(pos[0], "/") {
		return namespaceRemoveArgs{}, fmt.Errorf("usage: weft namespace remove NS [--force]")
	}
	return namespaceRemoveArgs{pos[0], on["force"]}, nil
}

// parseNamespaceCreateArgs: NAME.
func parseNamespaceCreateArgs(args []string) (string, error) {
	pos, _, _, err := scanArgs(args, nil, nil)
	if err != nil {
		return "", err
	}
	if len(pos) != 1 || strings.Contains(pos[0], "/") {
		return "", fmt.Errorf("usage: weft namespace create NAME")
	}
	return pos[0], nil
}
