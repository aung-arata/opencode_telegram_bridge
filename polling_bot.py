import requests
import os
import time

def load_env(path):
    try:
        with open(path) as f:
            for line in f:
                stripped = line.strip()
                if not stripped or stripped.startswith('#') or '=' not in stripped:
                    continue
                key, val = stripped.split('=', 1)
                key = key.strip()
                # Strip inline comments and surrounding whitespace/quotes from value
                val = val.split('#')[0].strip().strip('"\'')
                if key:
                    os.environ.setdefault(key, val)
    except Exception as e:
        print(f"Warning: could not load .env file: {e}")

import sys
load_env('.env')
BOT_TOKEN = os.environ.get('TG_BOT_TOKEN', '')
USER_ID = os.environ.get('TG_USER_ID', '')
API_URL = f'https://api.telegram.org/bot{BOT_TOKEN}'

# Validate config
if not BOT_TOKEN or not USER_ID:
    print("Error: TG_BOT_TOKEN and TG_USER_ID must be set in .env. Exiting.")
    sys.exit(1)

# Only commands from USER_ID will be accepted. Replies are sent to the triggering chat, so this works in private and group chats.

RUNTIME_DIR = 'runtime'
LAST_UPDATE_FILE = os.path.join(RUNTIME_DIR, 'last_update_id.txt')
# Stores polling bot state (safe to remove for reset)

# Ensure the runtime directory exists
os.makedirs(RUNTIME_DIR, exist_ok=True)


def get_updates(offset=None):
    url = f"{API_URL}/getUpdates"
    params = {'timeout': 5}
    if offset is not None:
        params['offset'] = offset
    try:
        resp = requests.get(url, params=params, timeout=10)
        if resp.status_code == 200:
            return resp.json().get('result', [])
        print(f"[get_updates] HTTP {resp.status_code}: {resp.text}")
    except Exception as e:
        print(f"Error getting updates: {e}")
    return []

def send_message(chat_id, text):
    url = f"{API_URL}/sendMessage"
    data = {'chat_id': chat_id, 'text': text}
    try:
        resp = requests.post(url, data=data, timeout=5)
        if resp.status_code != 200:
            print(f"[send_message] HTTP {resp.status_code} sending to chat_id={chat_id}: {resp.text}")
        else:
            result = resp.json()
            if not result.get('ok'):
                print(f"[send_message] Telegram API error: {result}")
    except Exception as e:
        print(f"Error sending message: {e}")

def load_last_update_id():
    try:
        with open(LAST_UPDATE_FILE) as f:
            return int(f.read().strip())
    except FileNotFoundError:
        return 0  # expected if no state yet
    except ValueError as ve:
        print(f"[load_last_update_id] Invalid contents in {LAST_UPDATE_FILE}: {ve}")
        return 0
    except OSError as oe:
        print(f"[load_last_update_id] OS error on {LAST_UPDATE_FILE}: {oe}")
        return 0
    # Let KeyboardInterrupt, SystemExit, etc. propagate

def save_last_update_id(update_id):
    tmp_file = LAST_UPDATE_FILE + '.tmp'
    try:
        with open(tmp_file, 'w') as f:
            f.write(str(update_id))
        os.replace(tmp_file, LAST_UPDATE_FILE)
    except Exception as e:
        print(f"Failed to save last update id: {e}")

def main():
    last_update_id = load_last_update_id()
    print("Bot started. Waiting for commands...")
    while True:
        updates = get_updates(offset=last_update_id + 1)
        max_update_id = last_update_id
        for update in updates:
            if 'message' in update:
                msg = update['message']
                sender = msg.get('from')
                if sender is None:
                    continue
                sender_id = str(sender['id'])
                chat_id = str(msg['chat']['id'])
                text = msg.get('text', '')

                if sender_id == USER_ID:
                    if text.startswith('/echo '):
                        reply = text[6:]
                        send_message(chat_id, reply)
                    elif text.strip() == '/help':
                        help_msg = ("OpenCode Telegram Bridge\n"
                                    "/echo <msg> - bot replies with your message\n"
                                    "/help - show this help")
                        send_message(chat_id, help_msg)
                # Optionally add else: send_message(chat_id, "Unauthorized")

            max_update_id = max(max_update_id, update['update_id'])
        if max_update_id != last_update_id:
            save_last_update_id(max_update_id)
            last_update_id = max_update_id
        time.sleep(2)

if __name__ == "__main__":
    main()
