(function(){
  var d=localStorage.getItem('theme')==='dark';
  var h=document.documentElement;
  h.setAttribute('data-color-mode',d?'dark':'light');
  h.setAttribute('data-dark-theme','dark');
  h.setAttribute('data-light-theme','light');
  var m=document.querySelector('meta[name="theme-color"]');
  if(m)m.content=d?'#0d1117':'#ffffff';
})();
