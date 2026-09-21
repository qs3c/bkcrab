#!/usr/bin/env python3
"""Launch an isolated evaluator with credentials from the normal provider store."""
import json
import os
from pathlib import Path
import subprocess
import sys

root=Path(sys.argv[1]).resolve()
name=sys.argv[2]
assert name.startswith('jev-bench-') and root.is_dir()
query="SELECT data FROM configs WHERE kind='provider' AND name='deepseek-official' AND user_id='' AND agent_id='' AND enabled=1"
read=subprocess.run(['docker','exec','-i','bkcrab-mysql-1','sh','-c','MYSQL_PWD="$MYSQL_PASSWORD" mysql -u"$MYSQL_USER" -D"$MYSQL_DATABASE" --default-character-set=utf8mb4 -N --batch --raw'],input=query,text=True,capture_output=True,check=True)
provider=json.loads(read.stdout)
assert provider['apiBase']=='https://api.deepseek.com/v1'
item=json.loads(subprocess.check_output(['docker','inspect','bkcrab-rag-evaluator-1']))[0]
env=os.environ.copy()
passed={}
for entry in item['Config']['Env']:
    k,_,v=entry.partition('=')
    if k.startswith('RAG_EVALUATOR_'):
        passed[k]=v
passed.update({'RAG_EVALUATOR_LLM_ENDPOINT':provider['apiBase'],'RAG_EVALUATOR_LLM_API_KEY':provider['apiKey'],
              'RAG_EVALUATOR_LLM_MODEL':'deepseek-v4-flash','RAG_EVALUATOR_LLM_INPUT_COST_USD_PER_MILLION':'0.3',
              'RAG_EVALUATOR_LLM_OUTPUT_COST_USD_PER_MILLION':'1.2','RAG_EVALUATOR_EMBEDDING_COST_USD_PER_MILLION':'0'})
env.update(passed)
cmd=['docker','run','-d','--name',name,'--network','bkcrab_default','--user',f'{os.getuid()}:{os.getgid()}',
     '--cpus','1','--memory','1536m','--read-only','--cap-drop','ALL','--security-opt','no-new-privileges',
     '--tmpfs','/tmp:rw,noexec,nosuid,size=128m','--mount',f'type=bind,src={root},dst=/experiment',
     '--mount',f'type=bind,src={Path(__file__).parent.resolve()},dst=/bench-scripts,readonly',
     '--workdir','/app','--env','PYTHONPATH=/app','--env','PYTHONDONTWRITEBYTECODE=1']
for key in passed:cmd+=['--env',key]
cmd+=['--entrypoint','python',item['Image'],'-u','/bench-scripts/direct_evaluate.py',*sys.argv[3:]]
subprocess.run(cmd,env=env,check=True)
