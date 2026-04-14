#!/bin/bash
# Bridge script for the shared user-data template.
# Weft's startup.sh handles both config fetching and service startup.
# This script is a no-op — startup.sh runs via weft-startup.service.
echo "fetch-config: weft uses startup.sh (weft-startup.service)"
