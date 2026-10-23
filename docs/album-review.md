# Ricostruzione degli album con foto già presenti

Il fork si avvia in modalità `review`: legge Google Photos e Immich, mostra
un'anteprima e crea l'album soltanto dopo la conferma. Non carica originali,
non modifica date e non elimina foto. La vecchia sincronizzazione rimane
disponibile soltanto avviando esplicitamente con `APP_MODE=sync`.

## Avvio con un Immich esistente

1. Creare `config.json` nella cartella del progetto con il contenuto seguente:

   ```json
   { "apiURL": "", "apiKey": "", "googlePhotos": [] }
   ```

2. Eseguire `docker compose -f compose.review.yml up -d --build`.
3. Aprire <http://localhost:8088> e salvare l'indirizzo Immich completo di `/api`
   e una chiave API dedicata. In Docker, `localhost` indica il container:
   per un Immich sul PC host usare `http://host.docker.internal:2283/api`.
4. Incollare il link condiviso Google Photos e generare l'anteprima.
5. Controllare titolo, candidati e date; selezionare le foto da includere.
6. Premere **Create album** oppure **Cancel**.

Se il titolo esiste già, l'app mostra gli album omonimi senza modificare Immich.
Selezionare l'album di destinazione e premere **Add to existing album** per
aggiungere le foto scelte, mantenendo quelle già presenti. La data di creazione,
il numero di foto (quando forniti da Immich) e l'ID aiutano a distinguere gli
album omonimi. Nessun album è selezionato automaticamente. Per creare invece un
album separato, premere **Use a different title** e cambiare il titolo.

Permessi della chiave: `asset.read`, `asset.view` (miniature), `album.read`,
`album.create`, `albumAsset.create`. La ricerca vede solo le foto accessibili
all'utente della chiave, comprese le restrizioni di condivisione di Immich.
Non serve `asset.upload`.

Se i risultati della ricerca compaiono ma le miniature Immich no, controllare
il permesso `asset.view`: `asset.read` consente la ricerca ma non autorizza
la visualizzazione delle immagini. La UI riporta il rifiuto HTTP 403 e il
permesso mancante. Dopo averlo abilitato sulla chiave, salvare in Immich e
ricaricare la pagina del plugin; non occorre ricreare l'anteprima.

L'interfaccia non ha autenticazione propria: i compose pubblicano solo su
127.0.0.1. Chi può aprire questa UI può usare la connessione configurata.
Per il binario nativo l'ascolto predefinito è 127.0.0.1; `LISTEN_HOST` permette
di cambiarlo quando si predispone un accesso autenticato esterno.

## Ambiente di test separato

Il compose in `dev/` usa un progetto Docker distinto, porte 8088 (plugin) e
2285 (Immich), senza cartelle o database della propria installazione abituale.
È basato sul compose della release ufficiale Immich v3.3.5. Il servizio di
machine learning è omesso: non serve per ricerca per nome, miniature e album.
La porta 8088 è condivisa con `compose.review.yml`: avviare un solo plugin
di prova alla volta.

In PowerShell, dalla cartella del progetto:

```powershell
Copy-Item dev/config.example.json dev/config.json
Copy-Item dev/.env.example dev/.env
# Impostare una password casuale per TEST_DB_PASSWORD nel file dev/.env.
docker compose --env-file dev/.env -f dev/compose.yml up -d --build
```

Aprire <http://localhost:2285>, creare un utente di prova, caricare una piccola
raccolta controllata e creare la chiave API. Inserire la chiave nell'interfaccia
del plugin a <http://localhost:8088>, mantenendo l'indirizzo interno
`http://immich-server:2283/api`.

`dev/.env`, `dev/config.json`, `config.json` e i dati sono esclusi da Git e dal
contesto di build Docker. Non sovrascrivere configurazioni esistenti.

Per fermare i servizi, conservando tutti i dati di prova:

```powershell
docker compose --env-file dev/.env -f dev/compose.yml down
```

## Regole di confronto

- Nome completo esatto, inclusa estensione e maiuscole/minuscole. La ricerca
  server viene verificata localmente per evitare risultati parziali del nome.
- Tutte le pagine di ricerca sono lette, senza filtri sulla data.
- La data Google è confrontata come istante UTC con la data EXIF di Immich,
  oppure con il suo `fileCreatedAt` se l'EXIF non è disponibile. Il timestamp
  di caricamento `createdAt` non è usato.
- Tolleranza di un secondo per la precisione dei metadati.
- Se manca una data, il candidato rimane visibile come **Check date**.
- Per le corrispondenze per nome, il primo candidato visualizzato è
  preselezionato, anche con data diversa, data assente o più risultati.
  Le corrispondenze con la stessa data
  vengono mostrate per prime. Si può scegliere un altro candidato o escludere
  la foto prima della conferma.
- Se la ricerca per nome non trova candidati e Google fornisce la data di
  scatto, viene eseguita una seconda ricerca nell'intervallo inclusivo di
  **±2 ore**. Le alternative possono avere nomi diversi e sono ordinate per
  vicinanza all'istante originale, confrontando i fusi orari correttamente.
  Foto e video vengono cercati separatamente. La UI le contrassegna come
  **Date alternatives (±2 hours)** e richiede di selezionarle manualmente:
  non sono preselezionate, nemmeno quando esiste un solo candidato.
  La ricerca legge tutte le pagine Immich e non modifica la libreria.
  Le foto senza candidati rimangono escluse.
- I nomi vengono letti con HEAD e Content-Disposition; nessun originale viene
  scaricato. Per i video si usa la variante `=dv`. Formati senza intestazione
  utilizzabile restano indicati come **Filename unavailable**.
- Una paginazione Google incompleta blocca l'anteprima anziché creare un album
  da un elenco parziale. Il protocollo Google è non ufficiale e può cambiare.

## Conferma, errori e riavvii

Le anteprime e lo stato di creazione sono salvati in `data/reviews/` con
identificatori casuali; trattare questa cartella come dati privati. Le chiavi API
non sono incluse nelle anteprime. Un cambio di connessione invalida le vecchie
anteprime e una anteprima non confermata scade dopo 24 ore. Eseguire una sola
istanza del plugin per directory dati.

Il POST di creazione non viene ritentato automaticamente. Se il server crea
l'album ma la risposta si perde, l'app chiede di verificare Immich: ripetere il
POST potrebbe creare un duplicato. Non eliminare il journal per forzare una
nuova creazione prima di questo controllo.

Se soltanto alcune aggiunte falliscono, la UI mostra
l'ID album e consente di ritentare **sullo stesso album**. Vengono controllati
gli esiti individuali, compresi i duplicati già presenti. Dopo l'inizio della
creazione o dell'integrazione, titolo, selezione e album di destinazione rimangono
fissi, anche dopo un riavvio. Per un album esistente, la destinazione viene
ricontrollata in Immich e salvata nel journal prima di aggiungere foto; un album
eliminato o rinominato dopo la proposta non viene sostituito automaticamente
con un nuovo album. Le foto già presenti non vengono rimosse e i risultati
`duplicate` dell'API sono considerati già aggiunti.

I doppi clic, le richieste contemporanee e la ripetizione dopo un riavvio sono
gestiti tramite lo stato persistente. Annullare è possibile prima della conferma;
non è un'operazione di eliminazione di un album creato o integrato.

## Verifiche di sviluppo

```powershell
go vet ./...
go test ./...
go test -race ./... # richiede un compilatore C compatibile
go build ./...

# Sola lettura dell'album, senza Immich e senza scaricare gli originali:
go run ./cmd/album-probe -url 'https://photos.app.goo.gl/LINK_DI_PROVA'

# Test live opzionale dei nomi: il link rimane nell'ambiente locale.
$env:GP2IMMICH_METADATA_URL = 'https://photos.app.goo.gl/LINK_DI_PROVA'
go test ./pkg/googlephotos -run TestIntegrationMetadataHeaders -count=1
Remove-Item Env:GP2IMMICH_METADATA_URL
```

I test ordinari usano fixture inventate e server simulati; non contengono link
o metadati privati. I test live preesistenti che scaricano media sono separati
e richiedono `GP2IMMICH_INTEGRATION=1`. GitHub esegue vet, test con race detector
e compilazione su Linux e Windows, poi costruisce e avvia il container per
controllare che la modalità predefinita sia review e che `/api/sync` sia bloccato.
La pubblicazione Docker dipende dal loro esito.

## Versione beta e ritorno alla precedente

Il workflow Docker pubblica sotto il proprietario del fork. Una prerelease
produce `beta`, una release stabile produce `latest`; entrambe hanno anche un
tag di versione. Per una prova riproducibile scegliere sempre il tag di versione
o il digest, non un tag mobile.

I push sul branch `dev`, dopo i controlli, pubblicano anche l'immagine di sviluppo
`ghcr.io/giuliobranchetti/gphotos2immich:dev` e `sha-<commit completo>`.
Questi push non aggiornano `latest` o `beta`. Se GitHub ha disabilitato Actions
nel fork, occorre abilitarle dalla scheda Actions del repository.

Conservare configurazione e journal prima di cambiare versione. Ritornare
all'immagine precedente non annulla gli album già creati. Tornando al progetto
originale può ripartire la sincronizzazione automatica dei vecchi album:
controllare prima configurazione e modalità di avvio.
