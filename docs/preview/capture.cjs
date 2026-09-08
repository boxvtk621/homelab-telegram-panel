// Documentation-only fixtures. Never connects to YouTrack or submits a comment.
const { chromium } = require('playwright');
const { createServer } = require('node:http');
const { readFile } = require('node:fs/promises');
const path = require('node:path');
const root = path.resolve(__dirname, '../..');
const out = path.join(root, 'docs/images');
const stamp = '2026-09-08T08:30:00.000Z';
const project = { id: '0-1', shortName: 'DEMO' };
const issues = ['Проверить восстановление резервной копии', 'Обновить карту домашних сервисов', 'Настроить уведомления о свободном месте'].map((summary, i) => ({ id: `2-${i+1}`, idReadable: `DEMO-${i+1}`, summary, updated: Date.parse(stamp), project, description: 'Восстановить тестовый архив в отдельный каталог.\nПроверить состав файлов и контрольные суммы.\nЗафиксировать результат проверки в обсуждении.', customFields: [{name:'Статус',value:{name:'В работе'}},{name:'Приоритет',value:{name:'Обычный'}}] }));
const articles = ['Карта сервисов HomeLab', 'Резервное копирование и восстановление', 'Проверка доступности сервисов'].map((summary,i) => ({id:`3-${i+1}`,idReadable:`DEMO-A-${i+1}`, summary, project,updated:Date.parse(stamp),content:'Демонстрационная статья для иллюстрации интерфейса.\n\nПеред работой проверьте доступность сервиса и актуальность резервной копии.'}));
const snapshot = data => ({data, observed_at:stamp});
(async () => {
  const server = createServer(async (req,res) => {
    const relative = new URL(req.url, 'http://localhost').pathname;
    if (relative.includes('..')) { res.writeHead(400).end(); return; }
    try {
      const file = path.join(root,'internal/mobilegatewayassets/dist',relative==='/'?'index.html':relative);
      const data = await readFile(file);
      res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html');
      res.end(data);
    } catch { res.writeHead(404).end(); }
  });
  await new Promise(resolve => server.listen(0,'127.0.0.1',resolve));
  let browser;
  try {
    browser = await chromium.launch({headless:true, ...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
    const context = await browser.newContext({viewport:{width:1280,height:960},deviceScaleFactor:1,locale:'ru-RU',timezoneId:'Europe/Samara',colorScheme:'light'});
    let authenticated = false;
    await context.route('**/*', async route => {
      const url = new URL(route.request().url());
      if (url.hostname !== '127.0.0.1') return route.abort();
      if (!url.pathname.startsWith('/api/')) return route.continue();
      if (route.request().method() !== 'GET') throw new Error('Preview must not write');
      const p = url.pathname.replace('/api/v2/','');
      let data;
      if(p==='session') {
        if(!authenticated) return route.fulfill({status:401,json:{error:'unauthorized'}});
        data={user:{id:'1-1',login:'demo',name:'Demo User'},csrf:'d'.repeat(43),writes_enabled:true,youtrack_url:'https://youtrack.example.invalid',project:'DEMO'};
      } else if(p==='issues') data=snapshot(issues);
      else if(p==='articles') data=snapshot(articles);
      else if(p.endsWith('/comments')) data=snapshot([{id:'4-1',text:'Тестовый архив подготовлен. Проверка выполняется в отдельном каталоге.',created:Date.parse(stamp),deleted:false,author:{name:'Demo User'}}]);
      else if(p.startsWith('issues/')) data=snapshot(issues.find(i=>i.idReadable===p.split('/')[1]));
      else if(p.startsWith('articles/')) data=snapshot(articles.find(i=>i.idReadable===p.split('/')[1]));
      else throw new Error(`Unknown preview route: ${p}`);
      return route.fulfill({json:data});
    });
    const page=await context.newPage();
    const errors=[];
    page.on('pageerror',e=>errors.push(e.message));
    const origin=`http://127.0.0.1:${server.address().port}`;
    const shot=async name=>{await page.evaluate(()=>document.fonts.ready);await page.screenshot({path:path.join(out,name+'.png'),fullPage:true});};
    await page.goto(origin); await page.getByRole('heading',{name:'Независимая веб-панель'}).waitFor(); await shot('login-desktop');
    authenticated=true; await page.reload(); await page.getByRole('button',{name:`DEMO-1 ${issues[0].summary}`,exact:true}).waitFor(); await shot('issues-desktop');
    await page.getByRole('button',{name:`DEMO-1 ${issues[0].summary}`,exact:true}).click(); await page.getByText('Тестовый архив подготовлен.',{exact:false}).waitFor(); await shot('issue-desktop');
    await page.setViewportSize({width:390,height:844}); await page.emulateMedia({colorScheme:'dark'});
    await page.getByRole('button',{name:'База знаний',exact:true}).click(); await page.getByRole('button',{name:`DEMO-A-1 ${articles[0].summary}`,exact:true}).waitFor(); await shot('knowledge-mobile-dark');
    if(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth)) throw new Error('Mobile horizontal overflow');
    const desktop=(await readFile(path.join(out,'issues-desktop.png'))).toString('base64');
    const mobile=(await readFile(path.join(out,'knowledge-mobile-dark.png'))).toString('base64');
    const overview=await browser.newPage({viewport:{width:1600,height:1000},deviceScaleFactor:1});
    await overview.setContent(`<!doctype html><html><meta charset="utf-8"><style>*{box-sizing:border-box}body{margin:0;background:#e9eee6;color:#183d2d;font-family:system-ui,sans-serif;padding:64px}small{letter-spacing:4px;font-size:18px}h1{font-size:64px;letter-spacing:-3px;margin:18px 0 10px}p{font-size:24px;color:#506757;margin:0}.desktop{position:absolute;left:64px;top:260px;width:900px;border:10px solid #263e31;border-radius:22px;box-shadow:0 28px 60px #153b2926;overflow:hidden}.desktop img{display:block;width:100%}.phone{position:absolute;right:130px;top:200px;width:308px;border:10px solid #1c2a21;border-radius:36px;overflow:hidden;box-shadow:0 30px 60px #153b2940}.phone img{display:block;width:100%}.note{position:absolute;bottom:28px;left:64px;font-size:17px;color:#56705e}</style><small>HOMELAB / WEB PANEL</small><h1>Рабочее место AI-агента</h1><p>Диалоги · Задачи · Контроль выполнения</p><div class="desktop"><img src="data:image/png;base64,${desktop}"></div><div class="phone"><img src="data:image/png;base64,${mobile}"></div><div class="note">Промежуточные экраны контекста · Демо-данные · Без диалога с агентом</div></html>`);
    await overview.evaluate(()=>document.fonts.ready); await overview.screenshot({path:path.join(out,'overview.png')});
    if(errors.length) throw new Error(errors.join('\n'));
    console.log('Captured 5 images; no page errors or mobile horizontal overflow. No upstream requests.');
  } finally { await browser?.close(); await new Promise(resolve=>server.close(resolve)); }
})().catch(e=>{console.error(e);process.exitCode=1;});
