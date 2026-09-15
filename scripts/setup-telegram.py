#!/usr/bin/env python3
"""Interactive, local-only credential setup. Never sends a Telegram message."""
import argparse
import getpass
import json
import os
from pathlib import Path
import re
import urllib.request
import warnings


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("redirect refused")


def api(token, method):
    # Do not log URLs or raw exceptions: Telegram embeds the credential in the URL.
    request = urllib.request.Request("https://api.telegram.org/bot" + token + "/" + method)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(request, timeout=10) as response:
        raw = response.read(1024 * 1024 + 1)
    if len(raw) > 1024 * 1024:
        raise ValueError("oversized response")
    result = json.loads(raw)
    if not result.get("ok"):
        raise ValueError("Telegram request rejected")
    return result["result"]


def private_write(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as output:
        output.write(value + "\n")


def channel_destination(token, channel_id):
    bot = api(token, "getMe")
    chat = api(token, f"getChat?chat_id={channel_id}")
    if chat.get("type") != "channel" or chat.get("id") != channel_id:
        raise ValueError("expected the requested channel")
    member = api(token, f"getChatMember?chat_id={channel_id}&user_id={int(bot['id'])}")
    if member.get("status") != "administrator" or not member.get("can_post_messages"):
        raise ValueError("bot needs permission to post in the channel")
    print(f"Channel ID: {channel_id}")
    if input("Save this channel as the Relay alert destination? [y/N]: ").strip().lower() != "y":
        raise ValueError("destination not confirmed")
    return channel_id


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--channel-id", type=int, help="Numeric Telegram channel ID")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    directory = root / ".local/telegram"
    if any((directory / name).exists() for name in ("bot_token", "chat_id")):
        raise ValueError("credentials already exist; rotate deliberately")
    warnings.simplefilter("error", getpass.GetPassWarning)
    token = getpass.getpass("Telegram bot token (hidden): ").strip()
    if not re.fullmatch(r"[0-9]+:[A-Za-z0-9_-]{20,}", token):
        raise ValueError("invalid token format")
    if args.channel_id is not None:
        selected = channel_destination(token, args.channel_id)
    else:
        api(token, "getMe")
        updates = api(token, "getUpdates?timeout=0&limit=100")
        chats = {}
        for update in updates:
            message = update.get("message", {})
            chat = message.get("chat", {})
            if str(message.get("text", "")).startswith("/start") and isinstance(chat.get("id"), int):
                chats[chat["id"]] = chat.get("type", "unknown")
        if not chats:
            print("No /start update found. Open your new bot, send /start, then rerun.")
            return
        print("Chats that sent /start:")
        for chat_id, kind in chats.items():
            print(f"  {chat_id} ({kind})")
        selected = int(input("Chat ID to receive Relay alerts: ").strip())
        if selected not in chats:
            raise ValueError("select a listed chat")
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    directory.chmod(0o700)
    private_write(directory / "bot_token", token)
    try:
        private_write(directory / "chat_id", str(selected))
    except BaseException:
        (directory / "bot_token").unlink()
        raise
    data = root / ".local/alertmanager-data"
    data.mkdir(mode=0o700, parents=True, exist_ok=True)
    print("Saved private credential files. No notifications have been enabled or sent.")


if __name__ == "__main__":
    try:
        main()
    except (Exception, KeyboardInterrupt):
        raise SystemExit("Setup did not complete. Check the token, /start message, chat selection and existing files; no secret details are printed.") from None
