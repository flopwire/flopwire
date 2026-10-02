# Re-capture direct CLI output using isolated synthetic sessions.
from pathlib import Path
import tempfile,os,subprocess,json,time
root=Path(__file__).resolve().parents[3]
fixtures=root/'landing/coordination-preview/fixtures'
tmp=Path(tempfile.mkdtemp(prefix='fw-site-',dir='/tmp'))
(tmp/'projects/acme-app').mkdir(parents=True);(tmp/'empty').mkdir()
for name,id in [('pagination','0b7e2c1a-0000-4000-8000-000000000001'),('api-change','79b2d8ef-0000-4000-8000-000000000001')]:
 (tmp/f'projects/acme-app/{id}.jsonl').write_bytes((fixtures/f'{name}.jsonl').read_bytes())
env={k:v for k,v in os.environ.items() if not k.startswith(('CLAUDE','CODEX','FLOPWIRE'))};env.update(FLOPWIRE_SESSION_ID='4c19e0d2-0000-4000-8000-000000000001',FLOPWIRE_AGENT='codex',FLOPWIRE_CONFIG=str(tmp/'config.json'),FLOPWIRE_INDEX=str(tmp/'index.db'))
import sys
bin=sys.argv[1] if len(sys.argv)>1 else '/tmp/flopwire-homepage-current'
def run(args):
 r=subprocess.run([bin,*args],env=env,cwd=root,text=True,capture_output=True,check=True);return r.stdout
print(run(['agent','run','--once','--no-sync','--claude-projects',str(tmp/'projects'),'--codex-home',str(tmp/'empty'),'--devin-db','-']))
captures={
 'grep-files.txt':['grep','-l','-F','next_cursor'],
 'search.txt':['search','next_cursor'],
 'grep.txt':['grep','-F','next_cursor'],
 'transcript-grep.txt':['grep','-n','-F','next_cursor'],
 'intended-grep.txt':['grep','-n','-F','next_cursor'],
 'read.txt':['read','0b7e2c1a-0000-4000-8000-000000000001/3026944:1','--messages-before','1'],
 'intended-read.txt':['read','0b7e2c1a-0000-4000-8000-000000000001/3026944:1','--messages-before','1'],
 'intended-sessions.json':['sessions','--repo','app','--branch','api-users'],
 'branch-session.json':['sessions','--repo','app','--branch','api-users'],
 'transcript-grep.json':['grep','-n','-F','next_cursor','--json'],
 'read.json':['read','0b7e2c1a-0000-4000-8000-000000000001/3026944:1','--messages-before','1','--json'],
}
for name,args in captures.items():
 out=run(args);(fixtures/name).write_text(out);print(name)


api='79b2d8ef-0000-4000-8000-000000000001';client='4c19e0d2-0000-4000-8000-000000000001'
env={k:v for k,v in os.environ.items() if not k.startswith(('CLAUDE','CODEX','FLOPWIRE'))}
env.update(FLOPWIRE_CONFIG=str(tmp/'config.json'),FLOPWIRE_INDEX=str(tmp/'index.db'),FLOPWIRE_SESSION_ID=client,FLOPWIRE_AGENT='codex')
api_path=tmp/f'projects/acme-app/{api}.jsonl'
records=[json.loads(x) for x in (fixtures/'api-change.jsonl').read_text().splitlines()]
from datetime import datetime,timedelta,timezone
for i,r in enumerate(records):r['timestamp']=(datetime.now(timezone.utc)-timedelta(seconds=10-i)).isoformat()
api_path.write_text(''.join(json.dumps(r)+'\n' for r in records))
codex_dir=tmp/'empty/sessions';codex_dir.mkdir(exist_ok=True)
ts=datetime.now(timezone.utc).isoformat()
cx=[{'timestamp':ts,'type':'session_meta','payload':{'id':client,'timestamp':ts,'cwd':'/work/acme/app','originator':'flopwire-fixture','cli_version':'1.0.0','source':'cli','model_provider':'fixture','git':{'branch':'main'}}},{'timestamp':ts,'type':'response_item','payload':{'type':'message','role':'user','content':[{'type':'input_text','text':'Update the profile client'}]}},{'timestamp':ts,'type':'event_msg','payload':{'type':'task_started','turn_id':'fixture-turn'}}]
(codex_dir/f'rollout-{client}.jsonl').write_text(''.join(json.dumps(r)+'\n' for r in cx))
(tmp/'sessions').mkdir(exist_ok=True);(tmp/f'sessions/{os.getpid()}.json').write_text(json.dumps({'pid':os.getpid(),'sessionId':api,'status':'busy','updatedAt':int(time.time()*1000)}))
sock=str(tmp/'bus.sock')
def run(args,who=client,stdin=None):
 e=env|{'FLOPWIRE_SESSION_ID':who,'FLOPWIRE_AGENT':'claude' if who==api else 'codex'}
 r=subprocess.run([bin,*args],cwd=root,env=e,text=True,input=stdin,capture_output=True,check=False)
 if r.returncode:raise RuntimeError(r.stderr+r.stdout)
 return r.stdout
log=open(tmp/'daemon.log','w');daemon=subprocess.Popen([bin,'agent','run','--no-sync','--socket',sock,'--claude-projects',str(tmp/'projects'),'--codex-home',str(tmp/'empty'),'--devin-db','-','--sweep','1s'],cwd=root,env=env,stdin=subprocess.DEVNULL,stdout=log,stderr=log)
try:
 for _ in range(100):
  if daemon.poll() is not None: raise RuntimeError((tmp/'daemon.log').read_text())
  try:
   peers=run(['peers','--socket',sock,'--repo','app'])
   if json.loads(peers).get('peers'):break
  except RuntimeError as e:
   pass
  time.sleep(.1)
 else:raise RuntimeError('presence timeout')
 (fixtures/'intended-peers.json').write_text(peers)
 sessions=run(['sessions','--repo','app','--branch','api-users'])
 for f in ['intended-sessions.json','branch-session.json']:(fixtures/f).write_text(sessions)
 (fixtures/'sessions-text.txt').write_text(run(['sessions','--repo','app','--branch','api-users','--text']))
 sent=run(['send',api,'--socket',sock,'--intent','request','--','My profile client reads name. Is full_name in api-users the final contract?'])
 (fixtures/'intended-send.json').write_text(sent);mid=json.loads(sent)['id']
 hook=run(['hook','--socket',sock],who=api,stdin=json.dumps({'hook_event_name':'PostToolUse','session_id':api}))
 (fixtures/'delivered-request.json').write_text(hook)
 reply=run(['send',client,'--socket',sock,'--reply-to',mid,'--intent','inform','--','Yes. Use full_name and test against api-users.'],who=api)
 (fixtures/'reply-receipt.json').write_text(reply)
 hook=run(['hook','--socket',sock],stdin=json.dumps({'hook_event_name':'PostToolUse','session_id':client}))
 (fixtures/'delivered-reply.json').write_text(hook)
 inbox=run(['inbox','--socket',sock,'--thread',mid]);(fixtures/'intended-inbox.json').write_text(inbox)
 print('Captured same-person fixture request, reply, and hook output.')
finally:
 daemon.terminate();daemon.wait(timeout=10);log.close()
