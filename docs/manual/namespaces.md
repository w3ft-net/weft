---
id: manual-namespaces
title: Namespaces
web:
  section: namespaces
  category: getting-started
  order: 10
---

## Create a namespace

Namespaces group related log sources. A typical setup has one namespace per
environment (e.g. `production`, `staging`) or per team.

Go to **Namespaces** in the sidebar and click **Create namespace**. Choose a
short, lowercase name — it becomes part of token identifiers and CLI scoping.

```
# From the shell
namespace create production
```
