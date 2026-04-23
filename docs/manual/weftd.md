---
id: manual-weftd
title: Install weftd
web:
  section: install-weftd
  order: 30
---

## Install weftd

weftd is a lightweight Go binary that tails log files and ships structured
lines to weft over gRPC. Download it from the **Downloads** page.

```
# Download and install
curl -fsSL https://w3ft.net/install.sh | sh

# Or on Debian/Ubuntu
sudo dpkg -i weftd_*.deb
```

Configure your log sources in `/etc/weftd/config.yaml`:

```
# /etc/weftd/config.yaml

endpoint: w3ft.net:443
token: wft_a1b2c3d4e5f6...

sources:
  - app: myapp
    path: /var/log/myapp/*.log
    schema: prefix_json
    prefix_fields: [ts, level, service]
    prefix_separator: " "
    json_field: payload

  - app: nginx
    path: /var/log/nginx/access.log
    schema: json
```

### Schema types

- `json` — pure JSON lines, no prefix
- `prefix_json` — fixed-width prefix fields followed by a JSON payload
- `raw` — unstructured text, no parsing

```
# Start weftd
sudo systemctl enable --now weftd

# Check status
sudo systemctl status weftd
journalctl -u weftd -f
```
