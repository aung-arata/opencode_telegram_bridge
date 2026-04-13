import datetime
import queue
import requests
import os
import shlex
import subprocess
import threading
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
                # Strip inline comments (only when preceded by whitespace) and surrounding whitespace
                val = val.strip()
                # Remove trailing inline comment: value must have a space before #
                if ' #' in val:
                    val = val[:val.index(' #')].strip()
                # Unquote values with matching surrounding quotes
                if len(val) >= 2 and val[0] == val[-1] and val[0] in ('"', "'"):
                    val = val[1:-1]
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

# OpenCode bridge configuration
OPENCODE_CMD = os.environ.get('OPENCODE_CMD', 'opencode')
# Seconds of stdout silence that signals the end of a response
OPENCODE_IDLE_TIMEOUT = float(os.environ.get('OPENCODE_IDLE_TIMEOUT', '2.0'))
# Hard cap: total seconds to wait for any response before giving up
OPENCODE_RESPONSE_TIMEOUT = int(os.environ.get('OPENCODE_RESPONSE_TIMEOUT', '30'))
LOG_FILE = os.path.join(RUNTIME_DIR, 'oc_bridge.log')
# Max characters of a response to write into the log line
LOG_RESPONSE_MAX_LEN = 200

# Ensure the runtime directory exists
os.makedirs(RUNTIME_DIR, exist_ok=True)

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

def log(msg):
    ts = datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
    line = f"[{ts}] {msg}"
    print(line)
    try:
        with open(LOG_FILE, 'a') as f:
            f.write(line + '\n')
    except Exception:
        pass

# ---------------------------------------------------------------------------
# OpenCode subprocess bridge
# ---------------------------------------------------------------------------

_oc_proc = None
_oc_lock = threading.Lock()
_oc_output_queue = queue.Queue()


def _oc_reader(proc):
    """Background thread: read OpenCode stdout line-by-line into the queue."""
    try:
        for line in proc.stdout:
            _oc_output_queue.put(line.rstrip('\n'))
    except Exception:
        pass
    _oc_output_queue.put(None)  # sentinel: process ended


def start_opencode():
    """Start the OpenCode subprocess if it is not already running."""
    global _oc_proc
    if _oc_proc is not None and _oc_proc.poll() is None:
        return True  # already running
    try:
        cmd = shlex.split(OPENCODE_CMD)
        _oc_proc = subprocess.Popen(
            cmd,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1,
        )
        t = threading.Thread(target=_oc_reader, args=(_oc_proc,), daemon=True)
        t.start()
        log(f"OpenCode started (pid={_oc_proc.pid}, cmd={OPENCODE_CMD!r})")
        return True
    except FileNotFoundError:
        log(f"OpenCode command not found: {OPENCODE_CMD!r}")
        return False
    except Exception as e:
        log(f"Failed to start OpenCode: {e}")
        return False


def query_opencode(text):
    """Send *text* to the OpenCode subprocess and return its response."""
    global _oc_proc
    with _oc_lock:

        # (Re-)start process if needed
        if _oc_proc is None or _oc_proc.poll() is not None:
            log("OpenCode not running, attempting to start…")
            if not start_opencode():
                return (
                    "❌ OpenCode is not available.\n"
                    "Make sure it is installed and OPENCODE_CMD is correct in .env."
                )

        # Drain any stale output left from a previous interaction
        while not _oc_output_queue.empty():
            try:
                _oc_output_queue.get_nowait()
            except queue.Empty:
                break

        # Write the query
        log(f"QUERY: {text}")
        try:
            _oc_proc.stdin.write(text + '\n')
            _oc_proc.stdin.flush()
        except Exception as e:
            log(f"Failed to write to OpenCode stdin: {e}")
            _oc_proc = None
            return "❌ OpenCode process closed unexpectedly. Please try again."

        # Collect response lines until idle for OPENCODE_IDLE_TIMEOUT seconds
        # or the hard deadline is reached
        lines = []
        deadline = time.time() + OPENCODE_RESPONSE_TIMEOUT
        while time.time() < deadline:
            try:
                line = _oc_output_queue.get(timeout=OPENCODE_IDLE_TIMEOUT)
                if line is None:  # process ended
                    log("OpenCode process ended during response")
                    _oc_proc = None
                    break
                lines.append(line)
            except queue.Empty:
                # No new output for idle_timeout seconds — response is complete
                break

        response = '\n'.join(lines).strip()
        if not response:
            response = "(no response from OpenCode)"
        log(f"RESPONSE ({len(lines)} lines): {response[:LOG_RESPONSE_MAX_LEN]}{'…' if len(response) > LOG_RESPONSE_MAX_LEN else ''}")
        return response


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

def send_message(chat_id, text, reply_to_message_id=None):
    url = f"{API_URL}/sendMessage"
    data = {'chat_id': chat_id, 'text': text}
    if reply_to_message_id is not None:
        data['reply_to_message_id'] = reply_to_message_id
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
                msg_id = msg.get('message_id')

                if sender_id == USER_ID:
                    if text.strip() == '/help':
                        help_msg = (
                            "OpenCode Telegram Bridge\n\n"
                            "Send any plain message → forwarded to local OpenCode as a query.\n"
                            "/ask <query>  — explicit OpenCode query\n"
                            "/echo <msg>   — bot replies with your message\n"
                            "/help         — show this help\n\n"
                            f"Response timeout: {OPENCODE_RESPONSE_TIMEOUT}s  "
                            f"idle cutoff: {OPENCODE_IDLE_TIMEOUT}s\n"
                            f"Log: {LOG_FILE}"
                        )
                        send_message(chat_id, help_msg, reply_to_message_id=msg_id)
                    elif text.startswith('/echo '):
                        send_message(chat_id, text[6:], reply_to_message_id=msg_id)
                    elif text.startswith('/ask '):
                        query = text[5:].strip()
                        if query:
                            response = query_opencode(query)
                            send_message(chat_id, response, reply_to_message_id=msg_id)
                    elif text and not text.startswith('/'):
                        # Plain message → OpenCode proxy
                        response = query_opencode(text)
                        send_message(chat_id, response, reply_to_message_id=msg_id)
                # Unauthorized senders are silently ignored

            max_update_id = max(max_update_id, update['update_id'])
        if max_update_id != last_update_id:
            save_last_update_id(max_update_id)
            last_update_id = max_update_id
        time.sleep(2)

if __name__ == "__main__":
    main()
