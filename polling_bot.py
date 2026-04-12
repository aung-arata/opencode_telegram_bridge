import requests
import os
import time

def load_env(path):
    try:
        with open(path) as f:
            for line in f:
                if line.strip() and not line.startswith('#'):
                    key, val = line.strip().split('=', 1)
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

LAST_UPDATE_FILE = 'last_update_id.txt'


def get_updates(offset=None):
    url = f"{API_URL}/getUpdates"
    params = {'timeout': 5}
    if offset:
        params['offset'] = offset
    try:
        resp = requests.get(url, params=params, timeout=10)
        if resp.status_code == 200:
            return resp.json().get('result', [])
    except Exception as e:
        print(f"Error getting updates: {e}")
    return []

def send_message(chat_id, text):
    url = f"{API_URL}/sendMessage"
    data = {'chat_id': chat_id, 'text': text}
    try:
        requests.post(url, data=data, timeout=5)
    except Exception as e:
        print(f"Error sending message: {e}")

def load_last_update_id():
    try:
        with open(LAST_UPDATE_FILE) as f:
            return int(f.read().strip())
    except:
        return 0

def save_last_update_id(update_id):
    try:
        with open(LAST_UPDATE_FILE, 'w') as f:
            f.write(str(update_id))
    except Exception as e:
        print(f"Failed to save last update id: {e}")

def main():
    last_update_id = load_last_update_id()
    print("Bot started. Waiting for commands...")
    while True:
        updates = get_updates(offset=last_update_id + 1)
        for update in updates:
            if 'message' in update:
                msg = update['message']
                sender_id = str(msg['from']['id'])
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

            # Always update last_update_id
            last_update_id = max(last_update_id, update['update_id'])
            save_last_update_id(last_update_id)
        time.sleep(2)

if __name__ == "__main__":
    main()
