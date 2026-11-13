#!/bin/sh
cd "$(dirname "$0")"
for dir in /opt/homebrew/bin /usr/local/bin /Library/Frameworks/Python.framework/Versions/Current/bin; do
  case ":$PATH:" in
    *":$dir:"*) ;;
    *) [ -d "$dir" ] && PATH="$PATH:$dir" ;;
  esac
done
export PATH
sh ./run-home-worker.sh "$@"
status=$?
if [ "$status" -ne 0 ]; then
  printf "Press Enter to close..."
  read answer || true
fi
exit "$status"
