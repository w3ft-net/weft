---
id: manual-connect-source
title: Overview
web:
  section: overview
  category: connect-source
  order: 20
---

## Connect a log source

The process for connecting a log source to weft is _mostly_ the same regardless
of whether you're using our own agent, pushing batches over HTTPS, or streaming
syslog over TCP: 

1. Create a token in the namespace of your choosing as described in [Namespaces
   & tokens](namespaces.md);
2. Feed that token to your log shipping mechanism;
3. Configure a schema to extract useful bits of information from your log lines
   (optional for source types we don't control; with our own agent, we enforce
   this part).

### Supported producers

* [weftd](weftd.md) is our homegrown log shipping agent. It's the fastest
  option and has a few more bells and whistles but you have to be able to
  actually run it -- not possible in every environment.
* [Cloudwatch+Kinesis](cloudwatch.md) -- our recommended path for monitoring
  AWS Lambdas.
* [Heroku](heroku.md) -- hey, Heroku is actually still in business, and we've
  still got some applications running there, so we got to support!
* [Render](render.md) -- our first foray into the new batch of Heroku successors.

## Sampling logs for a schema

Once a source starts producing lines, you'll see it in the "sources" under its
respective namespace. It will start off in a "sampling" state -- we capture a
50-line chunk to help to define a schema for that source. You can watch that
progress in the browser or with `weft source pending --watch`.

When using weftd, you're forced to define a schema for your log lines, (unless
the lines were written by a weft SDK, in which case then timestamp, level,
trace ID and spans come from each line's header). Though you don't have to
define a schema for other producers, we highly recommend it.  Having a means of
plucking [metavariables](metavars.md) out of the logs lets us make lookups on
those values nice and snappy. 

Once your log source has transitioned from "sampling" to "pending," you can
view our best guess about your sample in the browser or on the CLI with `weft
source configure`. A little wizard will walk you through our guesses for each
of the values we want to extract and you can approve, reject, etc.

You can resample at any time by clicking "resample" or running `weft source
resample`.
