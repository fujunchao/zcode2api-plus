/* zcode2api — 後台共用 UI 工具：HTML 跳脫、數值格式化、彈窗與確認框 */
function esc(s){return String(s==null?'':s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');}
function escAttr(s){return esc(s).replace(/"/g,'&quot;').replace(/'/g,'&#39;');}
function set(id,v){const e=document.getElementById(id);if(e)e.textContent=v;}
function fmt(v){return Number(v||0).toLocaleString('zh-TW');}
/* 緊湊數值：B/M 取兩位小數、k 取一位小數，其餘以千分位原樣顯示 */
function fmtCompact(v){const n=Number(v)||0;if(n>=1e9)return(n/1e9).toFixed(2)+'B';if(n>=1e6)return(n/1e6).toFixed(2)+'M';if(n>=1e3)return(n/1e3).toFixed(1)+'k';return fmt(n);}
/* epoch 秒 →「MM/DD HH:mm」 */
function fmtDate(ts){if(!ts)return'—';const d=new Date(ts*1000);return isNaN(d)?'—':d.toLocaleString('zh-TW',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'});}
/* 代理位址 → 顯示用的協定標籤 */
function proxyScheme(url){return String(url||'').split(':')[0].toLowerCase().startsWith('socks')?'SOCKS5':'HTTP';}
function openModal(id){document.getElementById(id).classList.add('open');}
function closeModal(id){document.getElementById(id).classList.remove('open');}
/* 確認框：頁面未放置 #modal-confirm 時自動注入 DOM；openConfirm(標題, HTML 內文, 確認回呼) */
var _cb=null;
function openConfirm(title,body,cb){
  if(!document.getElementById('modal-confirm')){
    document.body.insertAdjacentHTML('beforeend',
      '<div class="modal-overlay" id="modal-confirm"><div class="modal" style="max-width:400px">'
      +'<div class="modal-title" id="confirm-title">確認</div>'
      +'<div id="confirm-body" class="dialog-help"></div>'
      +'<div class="dialog-actions"><button onclick="closeModal(\'modal-confirm\')" class="dialog-btn">取消</button>'
      +'<button onclick="_cb&&_cb()" class="dialog-btn dialog-btn-danger">確認</button></div>'
      +'</div></div>');
  }
  _cb=async()=>{closeModal('modal-confirm');await cb();};
  set('confirm-title',title);
  document.getElementById('confirm-body').innerHTML=body;
  openModal('modal-confirm');
}
/* 點擊遮罩空白處關閉彈窗：事件委派一次綁定，對所有 .modal-overlay 生效 */
document.addEventListener('click',e=>{
  if(e.target&&e.target.classList&&e.target.classList.contains('modal-overlay'))e.target.classList.remove('open');
});
