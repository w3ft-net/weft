#!/bin/bash
# Bridge script for the shared user-data template.
# Weft services are started by startup.sh via weft-startup.service (enabled at boot).
# This script is a no-op to avoid double-starting.
ROLE="$1"
echo "start-role: weft-$ROLE started via weft-startup.service"
