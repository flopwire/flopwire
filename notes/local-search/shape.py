# Per-category byte accounting over the sampled files; extrapolates by size stratum.
import json,os,sys,collections
CAP=4096
def slen(x):
    if x is None: return 0
    if isinstance(x,str): return len(x.encode())
    if isinstance(x,list): return sum(slen(i) for i in x)
    if isinstance(x,dict):
        if x.get('type') in ('image','input_image') : return 0
        return sum(slen(v) for k,v in x.items() if k not in ('signature','encrypted_content','type','id','tool_use_id','call_id'))
    return 0
def imgbytes(x):
    s=json.dumps(x)
    return s.count('') and 0
def classify_codex(o,lb,acc):
    t=o.get('type'); p=o.get('payload') or {}; pt=p.get('type')
    if t=='compacted' or pt=='compacted': acc['compaction']+=lb; return
    if t=='event_msg':
        if pt in ('token_count','task_started','task_complete','turn_aborted'): acc['meta']+=lb
        else: acc['mirror_dup']+=lb
        return
    if t in ('session_meta','turn_context'): acc['meta']+=lb; return
    if t=='response_item':
        if pt=='message':
            role=p.get('role'); img=sum(len(json.dumps(c)) for c in p.get('content',[]) if c.get('type')=='input_image')
            acc['images']+=img
            txt=slen([c for c in p.get('content',[]) if c.get('type')!='input_image'])
            if role in('developer','system'): acc['instructions']+=lb-img
            else: acc['text']+=lb-img; acc['S_text']+=txt
            return
        if pt=='reasoning':
            acc['reasoning']+=lb; acc['S_reason']+=slen(p.get('summary'))+slen(p.get('content')); return
        if pt in('function_call','custom_tool_call','local_shell_call','web_search_call'):
            acc['tool_call']+=lb; acc['S_toolcall']+=min(CAP,slen(p.get('arguments') or p.get('input') or p.get('action'))); return
        if pt in('function_call_output','custom_tool_call_output'):
            acc['tool_out']+=lb; acc['S_toolout']+=min(CAP,slen(p.get('output'))); return
    acc['other']+=lb
def classify_claude(o,lb,acc):
    t=o.get('type'); m=o.get('message') or {}
    tur=o.get('toolUseResult'); tb=len(json.dumps(tur)) if tur is not None else 0
    if t not in('user','assistant'): acc['meta']+=lb; return
    cont=m.get('content')
    if isinstance(cont,str): acc['text']+=lb; acc['S_text']+=len(cont.encode()); return
    rest=lb; acc['mirror_dup']+=tb; rest-=tb
    for c in cont or []:
        cb=len(json.dumps(c)); ct=c.get('type')
        if ct=='image' or (ct=='tool_result' and 'base64' in json.dumps(c)[:20000] and '"image"' in json.dumps(c)[:20000]):
            acc['images']+=cb
        elif ct=='tool_result': acc['tool_out']+=cb; acc['S_toolout']+=min(CAP,slen(c.get('content')))
        elif ct=='tool_use': acc['tool_call']+=cb; acc['S_toolcall']+=min(CAP,slen(c.get('input')))
        elif ct in('thinking','redacted_thinking'): acc['reasoning']+=cb; acc['S_reason']+=slen(c.get('thinking'))
        elif ct=='text': acc['text']+=cb; acc['S_text']+=slen(c.get('text'))
        else: acc['other']+=cb
        rest-=cb
    acc['envelope']+=max(rest,0)
def analyze(p):
    acc=collections.Counter(); lens=[]
    iscodex='.codex/' in p
    with open(p,'rb') as f:
        for l in f:
            lb=len(l); lens.append(lb); acc['total']+=lb
            try: o=json.loads(l)
            except Exception: acc['bad']+=lb; continue
            (classify_codex if iscodex else classify_claude)(o,lb,acc)
    return acc,lens
if __name__=='__main__':
    d=json.load(open(os.path.join(os.path.dirname(os.path.abspath(__file__)),'sample.json')))
    buckets=[(0,100e3),(100e3,1e6),(1e6,5e6),(5e6,20e6),(20e6,1e12)]
    strat=collections.defaultdict(collections.Counter); alllens=[]
    for p in d['files']:
        s=os.path.getsize(p); src=next(x for x in ['.claude/','.codex/','devin-sessions'] if x in p)
        lo=[b for b in buckets if b[0]<=s<b[1]][0][0]
        a,lens=analyze(p); strat[f'{src}|{lo}']+=a; alllens+=lens
    alllens.sort(); n=len(alllens)
    print('lines',n,'p50',alllens[n//2],'p90',alllens[int(n*.9)],'p99',alllens[int(n*.99)],'max',alllens[-1])
    big=sum(x for x in alllens if x>100e3); print('share of bytes in lines >100KB',round(big/sum(alllens),3))
    # extrapolate: scale each stratum's category fractions by corpus stratum bytes
    tot=collections.Counter()
    for k,a in strat.items():
        W=d['weights'].get(k,0)
        if a['total']==0: continue
        for c,v in a.items(): tot[c]+=v/a['total']*W
    T=tot['total']
    print('corpus bytes (GB)',round(T/1e9,2))
    for c in ['text','tool_out','tool_call','reasoning','mirror_dup','compaction','images','instructions','envelope','meta','other','bad']:
        print(f'{c:13s} {tot[c]/T*100:5.1f}%  {tot[c]/1e9:6.2f} GB')
    S=tot['S_text']+tot['S_toolout']+tot['S_toolcall']+tot['S_reason']
    print('--- searchable extracted text (tool I/O capped 4KB)')
    for c in ['S_text','S_toolout','S_toolcall','S_reason']: print(f'{c:13s} {tot[c]/1e9:6.2f} GB')
    print(f'TOTAL searchable {S/1e9:.2f} GB = {S/T*100:.1f}% of raw; without reasoning {(S-tot["S_reason"])/1e9:.2f} GB')
    for src in ['.claude/','.codex/']:
        a=sum((strat[k] for k in strat if k.startswith(src)),collections.Counter())
        print(src,{c:round(a[c]/a['total']*100,1) for c in ['text','tool_out','tool_call','reasoning','mirror_dup','compaction','images','instructions','envelope','meta']})
