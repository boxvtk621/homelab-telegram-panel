// Local, synthetic browser acceptance only. No real node or YouTrack request.
// Run after the current Harness bundle is built; requires Playwright through NODE_PATH.
const { chromium } = require('playwright');
const { createServer } = require('node:http');
const { readFile, mkdir } = require('node:fs/promises');
const path = require('node:path');
const assert = require('node:assert/strict');
const { randomUUID } = require('node:crypto');
const controlsMode = process.env.HARNESS_SMOKE_SCENARIO === 'controls';
const root = path.resolve(__dirname, '../..');
const output = process.env.HARNESS_SMOKE_OUTPUT;
if (!output || !path.isAbsolute(output)) throw new Error('Set absolute HARNESS_SMOKE_OUTPUT outside the repository');
const nodeId = '20000000-0000-4000-8000-000000000001';
const dialogId = '30000000-0000-4000-8000-000000000001';
const csrf = 's'.repeat(43);
const assertNoOverflow = async (page,label) => {
  const layout=await page.evaluate(()=>({viewport:innerWidth,width:document.documentElement.scrollWidth,overflow:[...document.querySelectorAll('body *')].filter(el=>el.getBoundingClientRect().right>innerWidth).map(el=>({tag:el.tagName,id:el.id,class:el.className,right:el.getBoundingClientRect().right,width:el.getBoundingClientRect().width})).slice(0,20)}));
  assert.ok(layout.width<=layout.viewport,`${label} horizontal overflow: ${JSON.stringify(layout)}`);
};

(async () => {
  await mkdir(output, { recursive: true });
  const fixtures = JSON.parse(await readFile(path.join(root, 'api/harness-v1.fixtures.json'), 'utf8')).fixtures;
  const fixture = name => structuredClone(fixtures.find(f => f.name === name).value);
  const identity = fixture('read.identity');
  const snapshot = fixture('snapshot.atomic');
  const dialogs = fixture('page.dialogs');
  const history = fixture('page.history.user');
  const requests = fixture('page.requests');
  requests.items[0].status = 'completed';
  const attempts = fixture('page.attempts');
  attempts.items[0].state = 'completed';
  attempts.items[0].finishedAt = attempts.items[0].startedAt;
  const attemptEvents = fixture('page.events');
  attemptEvents.items.push(fixture('event.9.attempt.completed'));
  history.items[0].text = 'Синтетическая проверка резервной копии';
  history.items[0].disposition = 'applied';
  history.items.push(fixture('page.history.assistant').items[0]);
  history.items[1].content.content = 'Тестовый архив прочитан. Реальные сервисы в этой проверке не используются.';
  dialogs.items[0].title = 'Проверка архива';
  snapshot.pendingQueue=[];snapshot.node.pendingCount=0;snapshot.node.queuePaused=true;
  snapshot.node.blockedReasons=['operator_pause'];
  if (controlsMode) {
    requests.items[0].status='active';
    attempts.items[0].state='waiting_input';delete attempts.items[0].finishedAt;
    snapshot.activeAttempt=attempts.items[0];snapshot.node.activeAttemptId=attempts.items[0].attemptId;
    snapshot.node.occupancy='active';snapshot.node.queuePaused=false;snapshot.node.blockedReasons=[];
    attemptEvents.items=[fixture('event.6.attempt.started'),fixture('event.15.tool.started'),fixture('event.16.tool.output'),fixture('event.18.approval.requested'),fixture('event.20.input.requested')];
    attemptEvents.items[2].payload.output.content='Синтетический результат инструмента';
    attemptEvents.items[3].payload.safePrompt='Разрешить проверку тестового каталога?';
    attemptEvents.items[4].payload.prompt.content='Какой тестовый архив проверить?';
  }
  let releaseAck;
  const ackReady = new Promise(resolve => { releaseAck = resolve; });
  const commands = [];
  const streams = new Set();
  const replay = [];
  const errors = [];
  let authenticated = true;
  const stamp = () => new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');
  const refresh = () => {
    snapshot.capturedAt = stamp();
    for (const page of [dialogs, history, requests, attempts, attemptEvents]) {
      page.lastEventSeq = snapshot.lastEventSeq;
      page.snapshotStateVersion = snapshot.stateVersion;
    }
  };
  const json = (res, value, status = 200) => {
    res.writeHead(status, {'Content-Type':'application/json','Cache-Control':'no-store'});
    res.end(JSON.stringify(value));
  };
  const event = value => {
    const frame = `id: ${value.seq}\ndata: ${JSON.stringify(value)}\n\n`;
    replay.push({seq:value.seq, frame});
    for (const stream of streams) stream.write(frame);
  };
  const server = createServer(async (req, res) => {
    try {
      const url = new URL(req.url, 'http://127.0.0.1');
      const p = url.pathname;
      if (!p.startsWith('/api/')) {
        if (req.method !== 'GET' || p.includes('..')) return res.writeHead(400).end();
        const file = path.join(root, 'internal/mobilegatewayassets/dist', p === '/' ? 'index.html' : p);
        const bytes = await readFile(file);
        const media={'.js':'text/javascript','.css':'text/css','.png':'image/png','.svg':'image/svg+xml','.html':'text/html'};
        res.setHeader('Content-Type',media[path.extname(file)] ?? 'application/octet-stream');
        return res.end(bytes);
      }
      if (!authenticated) return json(res,{error:'authentication_required'},401);
      if (p === '/api/v2/session') return json(res,{user:{id:'1-1',login:'fixture',name:'Fixture Owner'},csrf,writes_enabled:true,youtrack_url:'https://youtrack.example.invalid',project:'DEMO'});
      if (p === '/api/v2/issues' || p === '/api/v2/articles') return json(res,{data:[],observed_at:stamp()});
      if (p === '/api/v2/agent/runs' && req.method === 'GET') return json(res,{runs:[],durable:false,model:'fixture'});
      if (p === '/api/v2/logout' && req.method === 'POST') {
        assert.equal(req.headers['x-panel-csrf'],csrf);authenticated=false;
        for (const stream of streams) stream.end();return json(res,{logged_out:true});
      }
      const base = '/api/v2/harness/nodes';
      if (p === base) return json(res,{registryVersion:1,mode:'fixture',nodes:[{nodeId,name:'Тестовая Cursor-нода',adapter:'cursor'}]});
      assert.ok(p.startsWith(`${base}/${nodeId}/`),`fixture forbids unknown node: ${req.method} ${p}`);
      const route = p.slice(`${base}/${nodeId}/`.length);
      refresh();
      if (route === 'identity') return json(res,identity);
      if (route === 'snapshot') return json(res,snapshot);
      if (route === 'dialogs') return json(res,dialogs);
      if (route === `dialogs/${dialogId}/messages`) return json(res,history);
      if (route === 'requests') {
        const page=structuredClone(requests);
        const state=url.searchParams.get('state');
        if(state)page.items=page.items.filter(item=>item.status===state);
        return json(res,page);
      }
      const attemptRoute=route.match(/^requests\/([^/]+)\/attempts$/);
      if(attemptRoute) {
        assert.ok(requests.items.some(item=>item.requestId===attemptRoute[1]),'unknown fixture request');
        const page=structuredClone(attempts);page.requestId=attemptRoute[1];
        if(page.requestId!==attempts.requestId)page.items=[];
        return json(res,page);
      }
      if(route===`attempts/${attempts.items[0].attemptId}/events`) {
        const page=structuredClone(attemptEvents);
        page.items=page.items.filter(item=>item.seq>Number(url.searchParams.get('after')??0));
        return json(res,page);
      }
      if(route===`attempts/${attempts.items[0].attemptId}`) {
        const item=fixture('read.attempt');item.attempt=attempts.items[0];item.stateVersion=snapshot.stateVersion;
        return json(res,item);
      }
      if (route === 'health/ready') {
        const health = fixture('health.ready');health.identity=identity;health.checkedAt=stamp();health.blockedReasons=snapshot.node.blockedReasons;health.readiness=health.blockedReasons.length?'blocked':snapshot.node.engineReadiness;return json(res,health);
      }
      if (route === 'events') {
        const after=Number(url.searchParams.get('after'));assert.ok(Number.isSafeInteger(after));
        res.writeHead(200,{'Content-Type':'text/event-stream','Cache-Control':'no-store'});res.write(': fixture\n\n');
        for (const entry of replay) if (entry.seq>after) res.write(entry.frame);
        streams.add(res);res.on('close',()=>streams.delete(res));return;
      }
      if (route === 'commands' && req.method === 'POST') {
        assert.equal(req.headers['x-panel-csrf'],csrf);
        const chunks=[];for await (const chunk of req) chunks.push(chunk);
        const command=JSON.parse(Buffer.concat(chunks).toString('utf8'));
        if (controlsMode) {
          assert.equal(command.target.nodeId,nodeId);assert.equal(command.target.attemptId,attempts.items[0].attemptId);
          assert.equal(command.expected.attemptGeneration,1);
          const names={'approval.respond':['receipt.8.approval.respond','event.19.approval.resolved'],'input.respond':['receipt.9.input.respond','event.21.input.resolved'],'attempt.stop':['receipt.5.attempt.stop','event.8.attempt.stop_requested']};
          assert.ok(names[command.kind],`unexpected control ${command.kind}`);
          commands.push(command);
          const receipt=fixture(names[command.kind][0]);const changed=fixture(names[command.kind][1]);
          if(command.kind==='approval.respond') {
            assert.equal(command.target.approvalId,'90000000-0000-4000-8000-000000000001');assert.equal(command.expected.approvalVersion,1);
            assert.deepEqual(command.payload,{decision:'allow_once',actionHash:'0'.repeat(64)});
          } else if(command.kind==='input.respond') {
            assert.equal(command.target.inputRequestId,'a0000000-0000-4000-8000-000000000001');assert.equal(command.expected.inputVersion,1);
            assert.equal(command.payload.text,'Только синтетический архив');
            const message=fixture('page.history.user').items[0];message.messageId=randomUUID();message.sequence=3;message.commandId=command.commandId;message.text=command.payload.text;message.disposition='applied';message.createdAt=stamp();history.items.push(message);
            receipt.references.messageId=message.messageId;changed.payload.messageId=message.messageId;
          } else {
            assert.deepEqual(command.payload,{});attempts.items[0].state='stopping';attempts.items[0].version++;
            snapshot.node.queuePaused=true;snapshot.node.queueVersion++;snapshot.node.blockedReasons=['operator_pause'];
            changed.payload.commandId=command.commandId;changed.entityVersion=attempts.items[0].version;
            receipt.blockingReason='operator_pause';
          }
          snapshot.stateVersion++;changed.seq=++snapshot.lastEventSeq;changed.observedAt=stamp();
          attemptEvents.items.push(changed);
          receipt.commandId=command.commandId;receipt.receiptId=randomUUID();receipt.acceptedAt=stamp();receipt.eventSeq=changed.seq;
          json(res,receipt,202);event(changed);return;
        }
        assert.equal(command.kind,'message.enqueue');assert.equal(command.target.nodeId,nodeId);assert.equal(command.target.dialogId,dialogId);
        assert.equal(command.expected.dialogVersion,1);
        commands.push(command);
        await ackReady;
        const message=fixture('page.history.user').items[0];
        message.messageId='40000000-0000-4000-8000-000000000003';message.requestId='50000000-0000-4000-8000-000000000002';
        message.commandId=command.commandId;message.sequence=3;message.text=command.payload.text;message.createdAt=stamp();history.items.push(message);
        const queued=fixture('snapshot.atomic').pendingQueue[0];queued.requestId=message.requestId;queued.inputMessageId=message.messageId;queued.queueSequence=2;snapshot.pendingQueue.push(queued);
        requests.items.push(structuredClone(queued));
        snapshot.node.pendingCount=snapshot.pendingQueue.length;snapshot.node.queueVersion++;snapshot.stateVersion++;dialogs.items[0].version++;
        const receipt=fixture('receipt.2.message.enqueue');receipt.commandId=command.commandId;receipt.acceptedAt=stamp();receipt.eventSeq=++snapshot.lastEventSeq;
        receipt.references.messageId=message.messageId;receipt.references.requestId=message.requestId;
        const accepted=fixture('event.3.message.accepted');accepted.seq=receipt.eventSeq;accepted.observedAt=stamp();accepted.entityId=message.messageId;
        accepted.payload.messageId=message.messageId;accepted.payload.requestId=message.requestId;accepted.payload.sequence=message.sequence;
        json(res,receipt,202);event(accepted);return;
      }
      throw new Error(`Unsupported fixture request ${req.method} ${route}`);
    } catch (error) {errors.push(error.message);if(!res.headersSent)res.writeHead(500);res.end();}
  });
  await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
  let browser;
  try {
    browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
    const context=await browser.newContext({viewport:{width:1280,height:1000},locale:'ru-RU',colorScheme:'light'});
    await context.route('**/*',route=>new URL(route.request().url()).hostname==='127.0.0.1'?route.continue():route.abort());
    const page=await context.newPage();page.on('pageerror',e=>errors.push(e.message));
    await page.goto(`http://127.0.0.1:${server.address().port}/`);
    await page.getByRole('button',{name:'Harness',exact:true}).click();
    await page.getByText('Fixture режим: данные синтетические', {exact:false}).waitFor();
    await page.getByLabel('Нода',{exact:true}).selectOption(nodeId);
    await page.getByRole('button',{name:/Проверка архива/}).click();
    if(controlsMode) {
      await page.getByRole('heading',{name:'Запрос разрешения',exact:true}).waitFor();
      await page.getByRole('heading',{name:'Агент ждёт ответ',exact:true}).waitFor();
      await page.getByText('Синтетический результат инструмента',{exact:true}).waitFor();
      await page.screenshot({path:path.join(output,'controls-desktop.png'),fullPage:true});
      await page.setViewportSize({width:390,height:844});await page.emulateMedia({colorScheme:'dark'});
      await page.screenshot({path:path.join(output,'controls-mobile-dark.png'),fullPage:true});
      await assertNoOverflow(page,'controls mobile');
      await page.getByRole('button',{name:'Разрешить один раз',exact:true}).click();
      await page.getByRole('heading',{name:'Запрос разрешения',exact:true}).waitFor({state:'hidden'});
      await page.getByRole('textbox',{name:/Ответ агенту/}).fill('Только синтетический архив');
      await page.getByRole('button',{name:'Ответить агенту',exact:true}).click();
      await page.getByRole('heading',{name:'Агент ждёт ответ',exact:true}).waitFor({state:'hidden'});
      await page.getByRole('button',{name:'Остановить попытку',exact:true}).click();
      await page.getByRole('button',{name:'Продолжить очередь',exact:true}).waitFor();
      await page.waitForFunction(()=>Array.from(document.querySelectorAll('#harness-attempt option')).some(option=>option.textContent.includes('stopping')));
      assert.equal(snapshot.activeAttempt.state,'stopping','cancel ACK was treated as terminal');
      assert.equal(snapshot.node.queuePaused,true,'stop lost manual pause');
      assert.deepEqual(commands.map(command=>command.kind),['approval.respond','input.respond','attempt.stop']);
      await page.screenshot({path:path.join(output,'controls-stopping-mobile.png'),fullPage:true});
      await page.reload();await page.getByRole('button',{name:'Harness',exact:true}).waitFor();
      assert.equal(commands.length,3,'reload resent a control');assert.deepEqual(errors,[]);
      console.log(JSON.stringify({status:'PASS',mode:'fixture',scenario:'controls',commands:commands.length,stopState:snapshot.activeAttempt.state,pageErrors:errors.length}));
      return;
    }
    await page.getByText('Тестовый архив прочитан.',{exact:false}).waitFor();
    const draft='Проверь целостность следующего тестового архива';
    await page.getByRole('textbox',{name:'Сообщение агенту'}).fill(draft);
    await page.getByRole('button',{name:'Отправить',exact:true}).click();
    await page.getByRole('button',{name:'Отправляем…',exact:true}).waitFor();
    assert.equal(await page.getByRole('textbox',{name:'Сообщение агенту'}).inputValue(),draft,'draft cleared before receipt');
    releaseAck();
    await page.getByText('Команда принята в очередь.',{exact:true}).waitFor();
    await page.getByText(draft,{exact:true}).waitFor();
    assert.equal(commands.length,1,'command resent automatically');
    assert.equal(await page.getByRole('textbox',{name:'Сообщение агенту'}).inputValue(),'');
    await page.screenshot({path:path.join(output,'harness-desktop.png'),fullPage:true});
    await page.setViewportSize({width:390,height:844});await page.emulateMedia({colorScheme:'dark'});
    await page.screenshot({path:path.join(output,'harness-mobile-dark.png'),fullPage:true});
    await assertNoOverflow(page,'mobile');
    await page.reload();
    assert.equal(commands.length,1,'page reload submitted a command');
    assert.deepEqual(errors,[]);
    console.log(JSON.stringify({status:'PASS',mode:'fixture',commands:commands.length,desktop:'harness-desktop.png',mobile:'harness-mobile-dark.png',pageErrors:errors.length}));
  } finally {
    releaseAck();await browser?.close();for(const stream of streams)stream.end();
    server.closeAllConnections();await new Promise(resolve=>server.close(resolve));
  }
})().catch(error=>{console.error(error);process.exitCode=1;});
