package notes

// The Apple Notes scripts are fixed JXA programs. Their input is one JSON
// object read from stdin, never part of the script text, so a note's title
// or body cannot change what a script does. Each prints one JSON object.

// jxaPrelude reads the stdin JSON object.
const jxaPrelude = `ObjC.import('Foundation');
function input() {
  var d = $.NSFileHandle.fileHandleWithStandardInput.readDataToEndOfFile;
  var s = ObjC.unwrap($.NSString.alloc.initWithDataEncoding(d, $.NSUTF8StringEncoding));
  return s ? JSON.parse(s) : {};
}
var Notes = Application('Notes');
`

// jxaList lists every folder's notes in bulk: one Apple Event per property
// per folder, not per note. A folder whose bulk read fails is read note by
// note, so one odd note cannot hide the rest.
const jxaList = jxaPrelude + `
function folderNotes(f) {
  var out = {folder: f.name(), ids: [], names: [], texts: [], locked: []};
  try {
    out.ids = f.notes.id();
    out.names = f.notes.name();
    out.locked = f.notes.passwordProtected();
    out.texts = f.notes.plaintext();
  } catch (e) {
    out.ids = []; out.names = []; out.texts = []; out.locked = [];
    var ns = f.notes();
    for (var i = 0; i < ns.length; i++) {
      try {
        var lk = ns[i].passwordProtected();
        out.ids.push(ns[i].id()); out.names.push(ns[i].name()); out.locked.push(lk);
        out.texts.push(lk ? '' : ns[i].plaintext());
      } catch (e2) {}
    }
  }
  return out;
}
function walk(f, acc) {
  acc.push(folderNotes(f));
  var subs = f.folders();
  for (var i = 0; i < subs.length; i++) walk(subs[i], acc);
}
function run() {
  input();
  var acc = [];
  var accounts = Notes.accounts();
  for (var a = 0; a < accounts.length; a++) {
    var fs = accounts[a].folders();
    for (var i = 0; i < fs.length; i++) walk(fs[i], acc);
  }
  return JSON.stringify({folders: acc});
}
`

// jxaRead reads one note by id.
const jxaRead = jxaPrelude + `
function run() {
  var a = input();
  var n = Notes.notes.byId(a.id);
  var name;
  try { name = n.name(); } catch (e) { return JSON.stringify({found: false}); }
  var locked = n.passwordProtected();
  var folder = '';
  try { folder = n.container().name(); } catch (e) {}
  return JSON.stringify({found: true, id: n.id(), name: name, locked: locked, folder: folder, text: locked ? '' : n.plaintext()});
}
`

// jxaCreate makes a note in the default account's default folder.
const jxaCreate = jxaPrelude + `
function run() {
  var a = input();
  var folder = Notes.defaultAccount().defaultFolder();
  var n = Notes.Note({body: a.html});
  folder.notes.push(n);
  return JSON.stringify({id: n.id()});
}
`

// jxaRecent lists notes created at or after a.since (milliseconds since
// the Unix epoch), for finding an earlier attempt's note.
const jxaRecent = jxaPrelude + `
function run() {
  var a = input();
  var ns = Notes.notes.whose({creationDate: {'>=': new Date(a.since)}});
  return JSON.stringify({ids: ns.id(), names: ns.name()});
}
`

// jxaCount counts the notes, for doctor.
const jxaCount = jxaPrelude + `
function run() {
  input();
  return JSON.stringify({count: Notes.notes.id().length});
}
`
