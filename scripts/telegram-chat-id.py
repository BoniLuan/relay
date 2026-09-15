#!/usr/bin/env python3
"""Look up a public channel ID without saving credentials or sending messages."""
import argparse
import getpass
import json
import re
import urllib.request
import warnings


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("redirect refused")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("channel", help="Public channel username, e.g. @chat_relay")
    args = parser.parse_args()
    channel = args.channel.removeprefix("@")
    if not re.fullmatch(r"[A-Za-z0-9_]+", channel):
        parser.error("use a channel username, not a URL")
    # Refuse getpass's echoing fallback when no interactive terminal is available.
    warnings.simplefilter("error", getpass.GetPassWarning)
    token = getpass.getpass("Telegram bot token (hidden): ").strip()
    if not re.fullmatch(r"[0-9]+:[A-Za-z0-9_-]{20,}", token):
        raise ValueError("invalid token format")
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({}), NoRedirect()
    )
    request = urllib.request.Request(
        "https://api.telegram.org/bot" + token + "/getChat?chat_id=%40" + channel
    )
    print("Querying Telegram (timeout: 10 seconds)...", flush=True)
    with opener.open(request, timeout=10) as response:
        raw = response.read(1024 * 1024 + 1)
    if len(raw) > 1024 * 1024:
        raise ValueError("oversized response")
    result = json.loads(raw)
    if not result.get("ok"):
        raise ValueError("request rejected")
    chat = result["result"]
    print("Chat ID:", chat["id"])


if __name__ == "__main__":
    try:
        main()
    except (Exception, KeyboardInterrupt):
        raise SystemExit(
            "Lookup failed. Check your terminal, token, channel username and bot access. "
            "No credentials were saved or messages sent."
        ) from None
