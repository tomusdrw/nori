# Backup i odtwarzanie serwisów w Nori

## Cel

Administrator może odzyskać serwis na obecnej lub nowej instancji Nori wraz z konfiguracją, sekretami i danymi aplikacji. Harmonogram i sposób pozyskania danych należą do serwisu. Lokalny zapis jest domyślny, a administrator instancji może dołączyć Backblaze B2.

## Podejście

- **Przenośne pełne archiwum per serwis** jest proponowanym formatem. Jeden plik wystarcza do ręcznego pobrania, wysyłki i importu.
- **Snapshot hosta** może być źródłem spójnej kopii plików w konfiguracji konkretnego serwisu. Wymaga wsparcia systemu plików lub dostawcy i zależy od układu wolumenów na hoście.
- **Repozytorium kopii przyrostowych** ograniczyłoby użycie dysku i transfer. Do ręcznego przeniesienia potrzebowałoby osobnego eksportu pełnej kopii oraz obsługi zależności między snapshotami. Można je dodać po zmierzeniu wielkości i częstotliwości kopii.

## Konfiguracja i harmonogram

- Każdy zwykły serwis, także już istniejący, dostaje domyślnie jedną lokalną kopię na dobę, godzinę rozłożoną między serwisami oraz retencję siedmiu ostatnich poprawnych kopii. Ustawienia serwisu pozwalają zmienić harmonogram, liczbę zachowanych kopii i włączyć tryb wyłącznie ręczny. Lokalne archiwum pozostaje podstawą pobierania, również po skonfigurowaniu B2.
- Harmonogram backupu jest niezależny od wdrożeń. Zmiany są widoczne bez restartu. Ręczne i zaplanowane kopie korzystają z tego samego mechanizmu. Zadania zajmujące się danymi lokalnymi są kolejkowane z ograniczoną współbieżnością. Nori koordynuje wdrożenia, kopiowanie i odtwarzanie tego samego serwisu, również dla serwisów Bash i niezależnie od punktu wejścia. Zaległe uruchomienia po restarcie są scalane do jednej kopii per serwis.
- Kopia obejmuje konfigurację z jednej zapisanej rewizji, w tym skrypt Bash albo konfigurację szablonu, oryginalny dokument dotenv, opis, ustawienia healthchecku i harmonogramy. Manifest odnotowuje digest uruchomionego obrazu, jeśli uda się go wiarygodnie ustalić. Pierwsza wersja obejmuje bieżącą konfigurację i stan danych serwisu. Pełny backup instancji Nori, historia wdrożeń, obrazy z rejestru i mechanizm PITR pozostają poza zakresem.

## Źródła danych i spójność

- Zarządzany szablon PostgreSQL domyślnie używa logicznego zrzutu bazy, wykonanego kompatybilnymi narzędziami PostgreSQL. Manifest przechowuje informacje potrzebne do sprawdzenia zgodności wersji i rozszerzeń. Import odtwarza bazę do nowego zarządzanego zasobu z tożsamością skonfigurowaną na instancji docelowej. Katalog roboczy PostgreSQL jest wykluczony z kopiowania plikowego przy takim zrzucie.
- Dla pozostałych danych Nori podpowiada montowania na podstawie kontenerów serwisu. Administrator zatwierdza konkretne wolumeny lub katalogi i może jawnie dodać inne źródła. W trybie Bash domyślna kopia obejmuje konfigurację, dopóki administrator nie wskaże danych. Panel pokazuje zakres i czas wykonania kopii każdego źródła.
- Per serwis można wybrać eksport podczas pracy, krótkie wstrzymanie zapisu na czas zbierania danych albo własną procedurę ze snapshotem. Eksport podczas pracy daje spójność w zakresie gwarancji danego źródła. Kiedy baza i pliki muszą odpowiadać jednej chwili, wybrana procedura musi wstrzymać wszystkie procesy zapisujące aż do utworzenia kompletu snapshotów lub eksportów. Dla szablonów Nori może zatrzymać własny kontener aplikacji i zostawić działający kontener PostgreSQL na potrzeby zrzutu. Dla serwisów Bash i dodatkowych procesów zapisujących administrator definiuje parę poleceń wstrzymania i wznowienia. Sama etykieta kontenera nie nadaje Nori prawa do jego zatrzymania. Pakowanie i wysyłka mogą odbyć się po wznowieniu aplikacji.
- Dla danych spoza obsługiwanych szablonów administrator może zdefiniować polecenia eksportu i importu. Eksport dostaje wydzielony katalog zadania na swoje pliki, np. wynik `VACUUM INTO` dla SQLite. Polecenia działają w kontekście administratora, z limitem czasu i bez ujawniania sekretów w statusach. Jeżeli wybrano procedurę wstrzymania, zadanie zapisuje jej stan przed zatrzymaniem. Wznowienie następuje także po błędzie lub anulowaniu, a przerwanie procesu Nori zostawia stan do podjęcia próby wznowienia po starcie. Kolejna operacja dla tego serwisu czeka na rozstrzygnięcie wznowienia.
- Niedostępność któregokolwiek włączonego źródła lub błąd procedury oznaczają nieudaną kopię. Panel pokazuje, na którym źródle zatrzymało się zadanie.

## Plik, retencja i alerty

- Jeden wersjonowany format zawiera manifest z typami źródeł, sumami kontrolnymi i payloadami. Szyfrowanie uwierzytelnione w trybie strumieniowym, proponowane z użyciem formatu age, chroni cały manifest i dane, w tym dotenv i skrypty. Domyślnie instancja generuje osobną tożsamość odzyskiwania podczas inicjalizacji, przechowuje ją z ograniczonym dostępem i umożliwia administratorowi wyeksportowanie klucza. Panel przypomina o zapisaniu klucza poza hostem. Instalacja może również używać dostarczonego klucza publicznego z tożsamością trzymaną poza Nori. Stare kopie wymagają klucza przypisanego do nich podczas tworzenia.
- Archiwa trafiają do trwałego katalogu backupów, domyślnie pod katalogiem danych Nori. Zapis i publikacja są oddzielne. Gotowy plik pojawia się w historii dopiero po pomyślnym ukończeniu i atomowej zmianie nazwy. Zadanie ma jawny stan także po restarcie. Niekompletne pliki są sprzątane, a retencja działa na kopiach ukończonych. Przerwanie backupu pozostawia ostatnią poprawną kopię.
- Monitor mierzy wolne miejsce na systemach plików używanych przez katalog danych Nori, archiwa i katalog roboczy, licząc wspólny system plików raz. W typowej instalacji zmierzy w ten sposób dysk hosta, na którym znajdują się dane i kopie. Administrator może dodać punkty pomiarowe dla innych istotnych dysków, np. osobnego dysku Docker, podając ścieżki widoczne dla Nori przez montowania tylko do odczytu. Panel wskazuje monitorowane systemy plików. Przy zdalnym daemonie Dockera pomiar z kontenera Nori dotyczy lokalnych montowań i nie obejmuje zdalnego dysku. Proponowany próg ostrzegawczy to poniżej 15% lub 5 GiB, z możliwością zmiany. Stan jest widoczny w panelu i wysyłany istniejącymi kanałami administratora, Telegramem lub SMS-em, jeśli je skonfigurowano. Alert pojawia się po wejściu w stan ostrzegawczy, z przypomnieniem najwyżej raz dziennie i komunikatem po powrocie powyżej progu. Deduplikacja przetrwa restart.
- Zadanie sprawdza dostępne miejsce przed rozpoczęciem i w czasie zapisu. Osobny konfigurowalny próg krytyczny zostawia rezerwę dla Nori i serwisów. Proponowana wartość to poniżej 5% lub 1 GiB. Po jego przekroczeniu nowe zadania są odraczane, a trwające zapisy kończą się kontrolowanym błędem i sprzątają pliki robocze. Retencja usuwa tylko kopie poza ustawionym limitem. W przypadku zewnętrznych poleceń eksportu monitor obejmuje ich katalog roboczy, a procedura administratora odpowiada za zapisy poza tym katalogiem.

## Backblaze B2

Pierwsza wersja wysyła gotowe lokalne pliki, więc włączenie B2 zachowuje ustawioną retencję lokalną. Tryb z samą retencją zdalną pozostaje poza zakresem. Administrator konfiguruje opcjonalny cel instancji z endpointem S3, regionem, prywatnym bucketem, prefiksem i kluczem aplikacji ograniczonym do bucketu. Dane dostępowe są szyfrowane w bazie Nori. Po włączeniu celu nowe kopie są domyślnie wysyłane, z możliwością wyłączenia uploadu dla danego serwisu. Panel pokazuje oddzielnie wynik kopii lokalnej i wysyłki. Przejściowa awaria wysyłki uruchamia ponowienia z opóźnieniem; administrator może też ponowić sam upload, póki lokalny plik jest dostępny. Retencję zdalną administrator ustawia niezależnie w regułach lifecycle B2, na prefiksie kopii. Import z B2 może korzystać z uprawnień wyłącznie do odczytu.

## Import i odtwarzanie

1. Administrator wybiera plik, lokalną kopię z historii lub obiekt w skonfigurowanym B2. Nori uwierzytelnia pełne archiwum, sprawdza wersję i integralność oraz przygotowuje podgląd bez wartości sekretów. Rozpakowanie ma limity wielkości i nie może wyjść poza katalog roboczy.
2. Administrator wskazuje nazwę docelową i mapowanie sieci, wolumenów oraz katalogów. Nori sprawdza dostęp do obrazu, zgodność bazy, kolizje, własność zasobów i miejsce na dysku. Przenoszony skrypt Bash można przejrzeć i poprawić pod kątem ścieżek i nazw właściwych dla docelowego hosta. Zaimportowane polecenia backupu lub restore wymagają jawnego zatwierdzenia.
3. Odtworzenie domyślnie tworzy osobny serwis z nowym ID, świeżymi zasobami i wyłączonym automatycznym wdrażaniem. Nori ponownie szyfruje dotenv kluczem instancji docelowej. Tam, gdzie Nori zarządza zasobami, nieudane odtworzenie sprząta wyłącznie zasoby nowej próby. Dla własnych poleceń administrator wskazuje świeże cele, a panel raportuje stan i efekty wymagające ręcznego sprzątania. Uruchomienie jest oddzielnym działaniem. Na tej samej instancji odzyskany serwis może zastąpić stary przez jawne przełączenie ruchu po sprawdzeniu danych. Pierwszy etap nie obejmuje nadpisywania zasobów aktywnego serwisu.
4. Panel pozwala wykonać próbne odtworzenie bez przełączania ruchu. Weryfikacja funkcji obejmuje import na czystej instancji z innym kluczem Nori, odzyskanie danych PostgreSQL i plików, błąd eksportu, mało miejsca, awarię B2 i restart podczas zadania.

W pierwszym wydaniu akcje dotyczące archiwów i kluczy są dostępne w panelu administratora. Istniejące uprawnienia MCP do edycji serwisów nie dają dostępu do plików backupu ani do uruchamiania procedur importu lub eksportu.

## Kolejność realizacji

1. Format archiwum, zapis lokalny, harmonogram, retencja, alerty o dysku oraz eksport i import konfiguracji z panelu.
2. Eksport i odtworzenie PostgreSQL, wybrane wolumeny i katalogi oraz procedury konfigurowane dla serwisu. Próba migracji całego serwisu na czystą instancję.
3. Wysyłka i import przez B2, ponowienia oraz sprawdzenie ścieżki odzyskiwania z obiektu zdalnego.
