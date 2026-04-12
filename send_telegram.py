import requests
import sys
import os

def load_env(path):
    try:
        with open(path) as f:
            for line in f:
                if line.strip() and not line.startswith('#'):
                    key, val = line.strip().split('=', 1)
                    os.environ.setdefault(key, val)
    except Exception as e:
        print(f"Warning: could not load .env file: {e}")

load_env('.env')
BOT_TOKEN = os.environ.get('TG_BOT_TOKEN', '')
CHAT_ID = os.environ.get('TG_CHAT_ID', '')

def send_telegram_message(msg):
    url = f"https://api.telegram.org/bot{BOT_TOKEN}/sendMessage"
    data = {"chat_id": CHAT_ID, "text": msg}
    resp = requests.post(url, data=data)
    print("Response:", resp.text)

if __name__ == "__main__":
    if len(sys.argv) > 1:
        send_telegram_message(" ".join(sys.argv[1:]))
    else:
        print("Usage: python3 send_telegram.py MESSAGE")
