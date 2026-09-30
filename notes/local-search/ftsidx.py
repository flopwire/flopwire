#!/usr/bin/env python3
"""ftsidx: streaming, incremental SQLite FTS5 sidecar over agent JSONL transcripts.

Each file is read from its last byte-offset watermark; only complete lines are
consumed. Extracted rows (user/assistant text, tool calls + outputs capped at
CAP bytes) go into a contentless FTS5 table whose rowid joins a small `msg`
provenance table (file, line_no, byte_offset, role, ts). Result text is
recovered by seeking into the raw file, so the index stores no copies.
Codex event_msg mirrors, compaction snapshots, images, Claude toolUseResult
duplicates and signatures are skipped.

usage: ftsidx.py DB index [FILES...|--roots]   |   ftsidx.py DB query 'terms' [--agent X] [--cwd PREFIX]
"""
import json, os, sqlite3, sys, time

CAP = 4096
ROOTS = ['~/.claude/projects', '~/.codex/sessions', '~/.agentboard/devin-sessions']
SCHEMA = """
PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;
CREATE TABLE IF NOT EXISTS files(id INTEGER PRIMARY KEY, path TEXT UNIQUE, ino INT, size INT,
  mtime REAL, off INT, line INT, agent TEXT, cwd TEXT, session TEXT, head BLOB);
CREATE TABLE IF NOT EXISTS msg(id INTEGER PRIMARY KEY, file INT, line INT, off INT, role TEXT, ts TEXT);
CREATE INDEX IF NOT EXISTS msg_file ON msg(file);
CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(body, content='', contentless_delete=1,
  tokenize="unicode61 remove_diacritics 2 tokenchars '_-./'");
"""

def cap(s):
    b = s.encode('utf-8', 'ignore')
    if len(b) <= CAP: return s
    return b[:CAP * 3 // 4].decode('utf-8', 'ignore') + ' … ' + b[-CAP // 4:].decode('utf-8', 'ignore')

def text_of(x):
    if x is None: return ''
    if isinstance(x, str): return x
    if isinstance(x, list): return '\n'.join(filter(None, (text_of(i) for i in x)))
    if isinstance(x, dict):
        t = x.get('type')
        if t in ('image', 'input_image', 'redacted_thinking'): return ''
        if 'text' in x and isinstance(x['text'], str): return x['text']
        if 'content' in x: return text_of(x['content'])
        if 'output' in x: return text_of(x['output'])
        return ''
    return ''

def extract_claude(o, st):
    t = o.get('type')
    if t not in ('user', 'assistant'): return
    if o.get('cwd') and not st.get('cwd'): st['cwd'] = o['cwd']
    if o.get('sessionId') and not st.get('session'): st['session'] = o['sessionId']
    ts = o.get('timestamp'); c = (o.get('message') or {}).get('content')
    if isinstance(c, str): yield t, ts, c; return
    for x in c or []:
        k = x.get('type')
        if k == 'text': yield t, ts, x.get('text', '')
        elif k == 'tool_use': yield 'tool_call', ts, x.get('name', '') + ' ' + cap(json.dumps(x.get('input'), ensure_ascii=False))
        elif k == 'tool_result': yield 'tool_out', ts, cap(text_of(x.get('content')))
        elif k == 'thinking' and x.get('thinking'): yield 'reasoning', ts, cap(x['thinking'])

def extract_codex(o, st):
    t = o.get('type'); ts = o.get('timestamp')
    p = o.get('payload') if t in ('response_item', 'session_meta') else o  # old format: top-level items
    if t == 'session_meta':
        st['cwd'] = st.get('cwd') or p.get('cwd'); st['session'] = st.get('session') or p.get('id'); return
    if t in ('event_msg', 'turn_context', 'compacted') or p is None: return
    pt = p.get('type')
    if pt == 'message':
        if p.get('role') in ('user', 'assistant'): yield p['role'], ts, text_of(p.get('content'))
    elif pt in ('function_call', 'custom_tool_call', 'local_shell_call'):
        yield 'tool_call', ts, (p.get('name') or '') + ' ' + cap(text_of(p.get('arguments') or p.get('input') or json.dumps(p.get('action'))))
    elif pt in ('function_call_output', 'custom_tool_call_output'):
        out = p.get('output'); out = out if isinstance(out, str) else text_of(out)
        yield 'tool_out', ts, cap(out)
    elif pt == 'reasoning':
        s = text_of(p.get('summary'))
        if s: yield 'reasoning', ts, cap(s)

def agent_of(path):
    return 'codex' if '/.codex/' in path else 'devin' if 'devin-sessions' in path else 'claude'

def index_file(db, path):
    try: stt = os.stat(path)
    except FileNotFoundError: return 0
    with open(path, 'rb') as f: head = f.read(512)
    row = db.execute('SELECT id,ino,size,mtime,off,line,cwd,session,head FROM files WHERE path=?', (path,)).fetchone()
    if row and row[1] == stt.st_ino and row[2] == stt.st_size and row[3] == stt.st_mtime: return 0
    reset = row is None or row[1] != stt.st_ino or stt.st_size < row[4] or row[8] != head[:len(row[8] or b'')]
    db.execute('BEGIN')
    if row is None:
        fid = db.execute('INSERT INTO files(path,agent) VALUES(?,?)', (path, agent_of(path))).lastrowid
        off, line, st = 0, 0, {}
    else:
        fid = row[0]
        if reset:  # truncated/rewritten: drop and rebuild this file only
            db.execute('DELETE FROM fts WHERE rowid IN (SELECT id FROM msg WHERE file=?)', (fid,))
            db.execute('DELETE FROM msg WHERE file=?', (fid,))
            off, line, st = 0, 0, {}
        else: off, line, st = row[4], row[5], {'cwd': row[6], 'session': row[7]}
    ext = extract_codex if agent_of(path) == 'codex' else extract_claude
    n = 0
    with open(path, 'rb') as f:
        f.seek(off)
        for raw in f:
            if not raw.endswith(b'\n'): break          # partial tail: wait for writer
            lo = off; off += len(raw); line += 1
            try: o = json.loads(raw)
            except ValueError: continue
            if not isinstance(o, dict): continue
            for role, ts, txt in ext(o, st):
                if not txt or not txt.strip(): continue
                mid = db.execute('INSERT INTO msg(file,line,off,role,ts) VALUES(?,?,?,?,?)', (fid, line, lo, role, ts)).lastrowid
                db.execute('INSERT INTO fts(rowid,body) VALUES(?,?)', (mid, txt)); n += 1
    db.execute('UPDATE files SET ino=?,size=?,mtime=?,off=?,line=?,cwd=?,session=?,head=? WHERE id=?',
               (stt.st_ino, stt.st_size, stt.st_mtime, off, line, st.get('cwd'), st.get('session'), head, fid))
    db.execute('COMMIT')
    return n

def walk_roots():
    for r in ROOTS:
        for d, _, fs in os.walk(os.path.expanduser(r)):
            for f in fs:
                if f.endswith('.jsonl'): yield os.path.join(d, f)

def snippet(path, off, q, width=160):
    with open(path, 'rb') as f:
        f.seek(off); raw = f.readline(2_000_000)
    s = raw.decode('utf-8', 'ignore'); i = max(0, s.lower().find(q.split()[0].strip('"').lower()))
    return s[max(0, i - 40): i + width].replace('\\n', ' ')

def to_fts(q):
    # bare words are quoted so punctuation (paste-buffer, a.b) is literal; raw FTS5 syntax passes through
    if any(k in q for k in ('"', ' AND ', ' OR ', ' NOT ', 'NEAR(', '*')): return q
    return ' '.join('"%s"' % w for w in q.split())

def query(db, q, agent=None, cwd=None, limit=10):
    q = to_fts(q)
    sql = ('SELECT f.path, m.line, m.off, m.role, m.ts, f.agent, f.cwd, bm25(fts) AS r FROM fts '
           'JOIN msg m ON m.id=fts.rowid JOIN files f ON f.id=m.file WHERE fts MATCH ?')
    args = [q]
    if agent: sql += ' AND f.agent=?'; args.append(agent)
    if cwd: sql += ' AND f.cwd LIKE ?'; args.append(cwd + '%')
    return db.execute(sql + ' ORDER BY r LIMIT ?', args + [limit]).fetchall()

if __name__ == '__main__':
    dbp, cmd, *rest = sys.argv[1:]
    db = sqlite3.connect(dbp, isolation_level=None); db.executescript(SCHEMA)
    if cmd == 'index':
        paths = list(walk_roots()) if rest == ['--roots'] else rest
        t0 = time.time(); n = sum(index_file(db, p) for p in paths)
        print(f'indexed {n} rows from {len(paths)} files in {time.time()-t0:.2f}s')
    elif cmd == 'query':
        q = rest[0]; kw = dict(zip(rest[1::2], rest[2::2]))
        t0 = time.perf_counter(); rows = query(db, q, kw.get('--agent'), kw.get('--cwd')); dt = time.perf_counter() - t0
        for r in rows: print(f'{r[0]}:{r[1]} @{r[2]} [{r[5]}/{r[3]}] {r[4]}\n   {snippet(r[0], r[2], q)[:160]}')
        print(f'{len(rows)} hits in {dt*1000:.1f} ms')
    elif cmd == 'optimize':
        db.execute("INSERT INTO fts(fts) VALUES('optimize')")
