import socket,json,sys,glob,os
pid=sys.argv[1]; text=sys.argv[2]; auth=len(sys.argv)>3 and sys.argv[3]=='auth'
reg=json.load(open(os.path.expanduser(f'~/.claude/sessions/{pid}.json')))
path=reg['messagingSocketPath']
s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM); s.connect(path)
buf=b''
if auth:
    k=json.load(open(glob.glob(os.path.expanduser(f'~/.claude/sessions/{pid}.*.key'))[0]))
    buf+=json.dumps({"type":"auth","token":k['peerToken']}).encode()+b'\n'
extra=json.loads(sys.argv[4]) if len(sys.argv)>4 else {}
msg={"type":"user","message":{"role":"user","content":text}}; msg.update(extra)
buf+=json.dumps(msg).encode()+b'\n'
s.sendall(buf); s.shutdown(socket.SHUT_WR)
try:
    s.settimeout(2); print('resp:',s.recv(4096))
except Exception as e: print('no resp',e)
