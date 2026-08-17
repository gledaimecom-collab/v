const getLanguage = (code) => {
  const lang = new Intl.DisplayNames(["en"], { type: "language" });
  return lang.of(code);
};

let settings = {
  enableProxy: false,
  proxyUrl: "",
  enableProwlarr: false,
  prowlarrHost: "",
  prowlarrApiKey: "",
  enableJackett: false,
  jackettHost: "",
  jackettApiKey: "",
};

const searchWrapper = document.querySelector("#search-wrapper");
var player = null;

function doubleTapFF(options) {
	var videoElement = this
	var videoElementId = this.id();
	document.getElementById(videoElementId).addEventListener("touchstart", tapHandler);
	var tapedTwice = false;
	function tapHandler(e) {
		if (!videoElement.paused()) {

			if (!tapedTwice) {
				tapedTwice = true;
				setTimeout(function () {
					tapedTwice = false;
				}, 300);
				return false;
			}
			e.preventDefault();
			var br = document.getElementById(videoElementId).getBoundingClientRect();


			var x = e.touches[0].clientX - br.left;
			var y = e.touches[0].clientY - br.top;

			if (x <= br.width / 2) {
				videoElement.currentTime(player.currentTime() - 10)
			} else {
				videoElement.currentTime(player.currentTime() + 10)

			}
		}


	}
}
videojs.registerPlugin('doubleTapFF', doubleTapFF);

(async function ($) {
  // toggle dark mode button
  const toggleDarkMode = () => {
    const html = document.querySelector("html");
    html.classList.toggle("dark");
    localStorage.setItem(
      "theme",
      html.classList.contains("dark") ? "dark" : "light"
    );
  };
  const toggleDarkModeButton = document.querySelector("#toggle_theme");
  toggleDarkModeButton.addEventListener("click", toggleDarkMode);

  // handle past button
  const pastButton = document.querySelector("#copy_magnet");
  pastButton.addEventListener("click", async () => {
    navigator.clipboard.readText().then((text) => {
      document.getElementById("magnet").value = text;
    });
  });

  // handle demo button
  const demoButton = document.querySelector("#demo_torrent");
  demoButton.addEventListener("click", async () => {
    document.getElementById("magnet").value =
      "magnet:?xt=urn:btih:08ada5a7a6183aae1e09d831df6748d566095a10&dn=Sintel&tr=udp%3A%2F%2Fexplodie.org%3A6969&tr=udp%3A%2F%2Ftracker.coppersurfer.tk%3A6969&tr=udp%3A%2F%2Ftracker.empire-js.us%3A1337&tr=udp%3A%2F%2Ftracker.leechers-paradise.org%3A6969&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337&tr=wss%3A%2F%2Ftracker.btorrent.xyz&tr=wss%3A%2F%2Ftracker.fastcast.nz&tr=wss%3A%2F%2Ftracker.openwebtorrent.com&ws=https%3A%2F%2Fwebtorrent.io%2Ftorrents%2F&xs=https%3A%2F%2Fwebtorrent.io%2Ftorrents%2Fsintel.torrent";

    document
      .querySelector("#torrent-form")
      .dispatchEvent(new Event("submit"));
  });

  const form = document.querySelector("#torrent-form");
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const magnet = document.querySelector("#magnet").value;

    if (!magnet) {
      butterup.toast({
        message: "Please enter a magnet link",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      return;
    }

    // clean up previous player
    if (player) {
      player.dispose();
      player = null;
      const vidElm = document.createElement("video");
      vidElm.setAttribute("id", "video-player");
      vidElm.setAttribute("class", "video-js mt-10 w-full");

      document.querySelector("main").appendChild(vidElm);
    }

    form
      .querySelector("button[type=submit]")
      .setAttribute("disabled", "disabled");
    form.querySelector("button[type=submit]").innerHTML = "";
    form.querySelector("button[type=submit]").classList.add("loader");

    const res = await fetch("/api/v1/torrent/add", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ magnet }),
    });

    if (!res.ok) {
      const err = await res.json();
      butterup.toast({
        message: err.error || "Something went wrong",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      form.querySelector("button[type=submit]").removeAttribute("disabled");
      form.querySelector("button[type=submit]").innerHTML = "Play Now";
      form.querySelector("button[type=submit]").classList.remove("loader");
      searchResults.querySelectorAll("#play-torrent").forEach((el) => {
        el.removeAttribute("disabled");
        el.innerHTML = "Watch";
        el.classList.remove("loader");
      });
      return;
    }

    const { sessionId } = await res.json();
    const filesRes = await fetch("/api/v1/torrent/" + sessionId);

    if (!filesRes.ok) {
      const err = await filesRes.json();
      butterup.toast({
        message: err.error || "Something went wrong",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      form.querySelector("button[type=submit]").removeAttribute("disabled");
      form.querySelector("button[type=submit]").innerHTML = "Play Now";
      form.querySelector("button[type=submit]").classList.remove("loader");
      document.querySelectorAll("#play-torrent").forEach((el) => {
        el.removeAttribute("disabled");
        el.innerHTML = "Watch";
        el.classList.remove("loader");
      });
      return;
    }

    const files = await filesRes.json();

    // Find video file
    const videoFiles = files.filter((f) =>
      f.name.match(/\.(mp4|mkv|webm|avi)$/i)
    );

    if (!videoFiles.length) {
      butterup.toast({
        message: "No video file found",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      form.querySelector("button[type=submit]").removeAttribute("disabled");
      form.querySelector("button[type=submit]").innerHTML = "Play Now";
      form.querySelector("button[type=submit]").classList.remove("loader");
      document.querySelectorAll("#play-torrent").forEach((el) => {
        el.removeAttribute("disabled");
        el.innerHTML = "Watch";
        el.classList.remove("loader");
      });
      return;
    }

    const subtitleFiles = files.filter((f) =>
      f.name.match(/\.(srt|vtt|sub)$/i)
    );

    const videoUrls = videoFiles.map((file) => {
      return {
        src: "/api/v1/torrent/" + sessionId + "/stream/" + file.index,
        title: file.name,
        type: "video/mp4",
      };
    });

    let subtitles = [];
    if (subtitleFiles.length) {
      subtitles = subtitleFiles.map((subFile) => {
        let language = "en";
        let langName = "English";

        // Try to extract language code from filename
        console.log(subFile.name);
        const langMatch = subFile.name.match(/\.([a-z]{2,3})\.(srt|vtt|sub)$/i);
        if (langMatch) {
          language = langMatch[1];
          langName = getLanguage(language);
        }

        return {
          src:
            "/api/v1/torrent/" +
            sessionId +
            "/stream/" +
            subFile.index +
            ".vtt?format=vtt",
          srclang: language,
          label: langName,
          kind: "subtitles",
          type: "vtt",
        };
      });
    }
    player = videojs(
      "video-player",
      {
        fluid: true,
        controls: true,
        autoplay: true,
        preload: "auto",
        sources: [{
          src: videoUrls[0].src,
          type: videoUrls[0].type,
          label: videoUrls[0].title,
        }],
        tracks: subtitles,
        html5: {
          nativeTextTracks: false
        },
        plugins: {
          hotkeys: {
            volumeStep: 0.1,
            seekStep: 5,
            enableModifiersForNumbers: false,
            enableVolumeScroll: false,
          },
        },
      },
      function () {
        player = this;
        player.on("error", (e) => {
          console.error(e);
          butterup.toast({
            message: "Something went wrong",
            location: "top-right",
            icon: true,
            dismissable: true,
            type: "error",
          });
        });
      }
    );
    player.doubleTapFF();

    document.querySelector("#video-player").style.display = "block";
    // scroll to video player
    setTimeout(() => {
      window.scrollTo({
        top: document.body.scrollHeight,
        behavior: "smooth",
      });

      if (videoUrls.length > 1) {
        const videoSelect = document.createElement("select");
        videoSelect.setAttribute("id", "video-select");
        videoSelect.setAttribute("class", "video-select");
        videoSelect.setAttribute("aria-label", "Select video");
        videoUrls.forEach((video) => {
          const option = document.createElement("option");
          option.setAttribute("value", video.src);
          option.innerHTML = video.title;
          videoSelect.appendChild(option);
        });
        videoSelect.addEventListener("change", (e) => {
          const selectedSrc = e.target.value;
          player.src({
            src: selectedSrc,
            type: "video/mp4",
          });
          player.play();
        });
        document.querySelector("#video-player").appendChild(videoSelect);
      }
      player.play()
    }, 300);

    form.querySelector("button[type=submit]").removeAttribute("disabled");
    form.querySelector("button[type=submit]").innerHTML = "Play Now";
    form.querySelector("button[type=submit]").classList.remove("loader");
    document.querySelectorAll("#play-torrent").forEach((el) => {
      el.removeAttribute("disabled");
      el.innerHTML = "Watch";
      el.classList.remove("loader");
    });
  });

  // create switch button
  const switchInputs = document.querySelectorAll("#switchInput");
  switchInputs.forEach((input) => {
    input.querySelector("input").addEventListener("change", (e) => {
      const dot = e.target.parentElement.querySelector(".dot");
      const wrapper = e.target.parentElement.querySelector(".switch-wrapper");
      if (e.target.checked) {
        dot.classList.add("translate-x-full", "!bg-muted");
        wrapper.classList.add("bg-primary");
      } else {
        dot.classList.remove("translate-x-full", "!bg-muted");
        wrapper.classList.remove("bg-primary");
      }
    });
  });

  document.querySelector("#settings-btn").addEventListener("click", () => {
    document.querySelector("#settings-model").classList.toggle("hidden");
  });

  document.querySelectorAll("#close-settings").forEach((el) => {
    el.addEventListener("click", () => {
      document.querySelector("#settings-model").classList.toggle("hidden");
      document.querySelector("#proxy-result").classList.remove("flex");
    document.querySelector("#proxy-result").classList.add("hidden");
    });
  });

  document.querySelectorAll(".tab-btn").forEach((el) => {
    el.addEventListener("click", () => {
      const tabIndex = el.getAttribute("data-index");
      document.querySelectorAll(".tab").forEach((tab) => {
        const index = tab.getAttribute("data-tab");
        if (index === tabIndex) {
          tab.classList.remove("hidden");
          document.querySelectorAll(".tab-btn").forEach((el) => {
            el.classList.remove("bg-primary", "text-primary-foreground");
            el.classList.add("bg-muted");
          });
          el.classList.add("bg-primary", "text-primary-foreground");
        } else {
          tab.classList.add("hidden");
        }
      });
    });
  });

  function generatePagination(currentPage, pageSize, total, target) {
    const pagination = document.querySelector(target);
    if (!pagination) return;
    pagination.classList.remove("hidden");
    pagination.innerHTML = "";
    const totalPages = Math.ceil(total / pageSize);
    const startPage = Math.max(1, currentPage - 2);
    const endPage = Math.min(totalPages, currentPage + 2);

    for (let i = startPage; i <= endPage; i++) {
      const pageButton = document.createElement("button");
      pageButton.textContent = i;
      pageButton.classList.add("page-button");
      if (i === currentPage) {
        pageButton.classList.add("active");
      }
      pageButton.addEventListener("click", () => {
        searchPage = i;
        updateSearchResults();
      });
      pagination.appendChild(pageButton);
    }
    const prevButton = document.createElement("button");
    prevButton.innerHTML = `<svg stroke="currentColor" fill="currentColor" stroke-width="0" viewBox="0 0 20 20" aria-hidden="true" height="1em" width="1em" xmlns="http://www.w3.org/2000/svg"><path fill-rule="evenodd" d="M4.72 9.47a.75.75 0 0 0 0 1.06l4.25 4.25a.75.75 0 1 0 1.06-1.06L6.31 10l3.72-3.72a.75.75 0 1 0-1.06-1.06L4.72 9.47Zm9.25-4.25L9.72 9.47a.75.75 0 0 0 0 1.06l4.25 4.25a.75.75 0 1 0 1.06-1.06L11.31 10l3.72-3.72a.75.75 0 0 0-1.06-1.06Z" clip-rule="evenodd"></path></svg>`;
    prevButton.classList.add("page-button");
    prevButton.disabled = currentPage === 1;
    prevButton.addEventListener("click", () => {
      if (currentPage > 1) {
        searchPage--;
        updateSearchResults();
      }
    });
    pagination.prepend(prevButton);
    const nextButton = document.createElement("button");
    nextButton.innerHTML = `<svg stroke="currentColor" fill="currentColor" stroke-width="0" viewBox="0 0 20 20" aria-hidden="true" height="1em" width="1em" xmlns="http://www.w3.org/2000/svg"><path fill-rule="evenodd" d="M15.28 9.47a.75.75 0 0 1 0 1.06l-4.25 4.25a.75.75 0 1 1-1.06-1.06L13.69 10 9.97 6.28a.75.75 0 0 1 1.06-1.06l4.25 4.25ZM6.03 5.22l4.25 4.25a.75.75 0 0 1 0 1.06l-4.25 4.25a.75.75 0 0 1-1.06-1.06L8.69 10 4.97 6.28a.75.75 0 0 1 1.06-1.06Z" clip-rule="evenodd"></path></svg>`;
    nextButton.classList.add("page-button");
    nextButton.disabled = currentPage === totalPages;
    nextButton.addEventListener("click", () => {
      if (currentPage < totalPages) {
        searchPage++;
        updateSearchResults();
      }
    });
    pagination.appendChild(nextButton);
  }

  let searchData = [];
  let searchPage = 1;
  let searchPageSize = 5;

  const updateSearchResults = () => {
    const searchPagination = document.querySelector("#search-pagination");
    const searchResults = document.querySelector("#search-result");
    searchResults.classList.remove("hidden");
    searchResults.querySelector("tbody").innerHTML = "";
    searchResults.querySelector("tfoot").classList.add("hidden");
    if (searchData.length === 0) {
      searchResults.querySelector("tfoot").classList.remove("hidden");
      return;
    }

    const start = (searchPage - 1) * searchPageSize;
    const end = start + searchPageSize;
    const results = searchData.slice(start, end);
    results.forEach((result) => {
      const resultDiv = document.createElement("tr");
      resultDiv.innerHTML = `
        <td>${result.title}</td>
        <td>${result.indexer}</td>
        <td>${result.size}</td>
        <td>${result.leechers}/${result.seeders}</td>
        <td><button id="play-torrent" type="button" class="btn small" data-magnet="${
          result.downloadUrl || result.magnetUrl
        }">Watch</button></td>
      `;
      searchResults.querySelector("tbody").appendChild(resultDiv);
    });

    // Generate pagination
    const totalResults = searchData.length;
    const totalPages = Math.ceil(totalResults / searchPageSize);
    generatePagination(
      searchPage,
      searchPageSize,
      totalResults,
      "#search-pagination"
    );

    // Add event listener to each play button
    searchResults.querySelectorAll("#play-torrent").forEach((el) => {
      el.addEventListener("click", async (e) => {
        const magnet = e.target.getAttribute("data-magnet");
        document.querySelector("#magnet").value = magnet;
        document
          .querySelector("#torrent-form")
          .dispatchEvent(new Event("submit"));
        e.target.setAttribute("disabled", "disabled");
        e.target.innerHTML = "";
        e.target.classList.add("loader");
      });
    });
  };

  document.querySelector("#search-form").addEventListener("submit", (e) => {
    e.preventDefault();
    const query = e.target.querySelector("#search").value;
    if (!query) {
      butterup.toast({
        message: "Please enter a search query",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      return;
    }

    searchData = [];
    searchPage = 1;

    e.target
      .querySelector("button[type=submit]")
      .setAttribute("disabled", "disabled");
    e.target.querySelector("button[type=submit]").classList.add("loader");
    e.target.querySelector("button[type=submit]").innerHTML = "";
    const searchResults = document.querySelector("#search-result");

    searchResults.classList.add("hidden");
    document.querySelector("#search-pagination").classList.add("hidden");

    let apiUrl = "/api/v1/prowlarr/search";

    if (
      (!settings.prowlarrHost || !settings.prowlarrApiKey) &&
      settings.jackettHost &&
      settings.jackettApiKey
    ) {
      apiUrl = "/api/v1/jackett/search";
    }

    fetch(`${apiUrl}?q=${query}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
    })
      .then(async (res) => {
        if (!res.ok) {
          const err = await res.json();
          throw new Error(res.error || "Failed to fetch search results");
        }
        return res.json();
      })
      .then((data) => {
        if (data && typeof data === "object") {
          searchData = data;
        } else {
          searchData = [];
        }

        updateSearchResults();
      })
      .catch((error) => {
        console.error("There was a problem with the fetch operation:", error);
        butterup.toast({
          message: error.message || "Failed to fetch search results",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "error",
        });
      })
      .finally(() => {
        e.target
          .querySelector("button[type=submit]")
          .removeAttribute("disabled");
        e.target
          .querySelector("button[type=submit]")
          .classList.remove("loader");
        e.target.querySelector("button[type=submit]").innerHTML = "Search";
      });
  });

  const testProwlarrConfig = async () => {
    const prowlarrHost = document.querySelector("#prowlarrHost").value;
    const prowlarrApiKey = document.querySelector("#prowlarrApiKey").value;
    const prowlarrTestBtn = document.querySelector("#test-prowlarr");

    if (!prowlarrHost || !prowlarrApiKey) {
      butterup.toast({
        message: "Please enter Prowlarr host and API key",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      return false;
    }

    prowlarrTestBtn.setAttribute("disabled", "disabled");
    prowlarrTestBtn.querySelector("span").innerHTML = "Testing...";
    
    const response = await fetch("/api/v1/prowlarr/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prowlarrHost, prowlarrApiKey }),
    });

    const data = await response.json();
    if (!response.ok) {
      butterup.toast({
        message: data.error || "Failed to test Prowlarr connection",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      prowlarrTestBtn.removeAttribute("disabled");
      prowlarrTestBtn.querySelector("span").innerHTML = "Test Connection";
      return false;
    }

    butterup.toast({
      message: "Prowlarr settings are valid",
      location: "top-right",
      icon: true,
      dismissable: true,
      type: "success",
    });

    prowlarrTestBtn.removeAttribute("disabled");
    prowlarrTestBtn.querySelector("span").innerHTML = "Test Connection";

    return true;
  }

  document.querySelector("#test-prowlarr").addEventListener("click", (e) => {
    testProwlarrConfig();
  });

  const testJackettConfig = async () => {
    const jackettHost = document.querySelector("#jackettHost").value;
    const jackettApiKey = document.querySelector("#jackettApiKey").value;
    const jackettTestBtn = document.querySelector("#test-jackett");

    if (!jackettHost || !jackettApiKey) {
      butterup.toast({
        message: "Please enter Jackett host and API key",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      return false;
    }

    jackettTestBtn.setAttribute("disabled", "disabled");
    jackettTestBtn.querySelector("span").innerHTML = "Testing...";
    
    const response = await fetch("/api/v1/jackett/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ jackettHost, jackettApiKey }),
    });

    const data = await response.json();
    if (!response.ok) {
      butterup.toast({
        message: data.error || "Failed to test Jackett connection",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      jackettTestBtn.removeAttribute("disabled");
      jackettTestBtn.querySelector("span").innerHTML = "Test Connection";
      return false;
    }

    butterup.toast({
      message: "Jackett settings are valid",
      location: "top-right",
      icon: true,
      dismissable: true,
      type: "success",
    });

    jackettTestBtn.removeAttribute("disabled");
    jackettTestBtn.querySelector("span").innerHTML = "Test Connection";

    return true;
  }

  document.querySelector("#test-jackett").addEventListener("click", (e) => {
    testJackettConfig();
  });

  const testProxy = async () => {
    const proxyUrl = document.querySelector("#proxyUrl").value;
    const proxyBtn = document.querySelector("#test-proxy");

    if (!proxyUrl) {
      butterup.toast({
        message: "Please enter a proxy URL",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      return false;
    }

    proxyBtn.setAttribute("disabled", "disabled");
    proxyBtn.querySelector("span").innerHTML = "Testing...";

    const response = await fetch("/api/v1/proxy/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ proxyUrl }),
    });

    const data = await response.json();

    if (!response.ok) {
      butterup.toast({
        message: data.error || "Failed to test Proxy connection",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      proxyBtn.removeAttribute("disabled");
      proxyBtn.querySelector("span").innerHTML = "Test Proxy";
      return false;
    }

    butterup.toast({
      message: "Proxy url is valid",
      location: "top-right",
      icon: true,
      dismissable: true,
      type: "success",
    });

    proxyBtn.removeAttribute("disabled");
    proxyBtn.querySelector("span").innerHTML = "Test Proxy";

    if (data?.origin) {
      document.querySelector("#proxy-result").classList.remove("hidden");
      document.querySelector("#proxy-result").classList.add("flex");
      document.querySelector("#proxy-result .output-ip").innerHTML = data?.origin
    }

    return true;
  }

  document.querySelector("#test-proxy").addEventListener("click", () => {
    testProxy();
  });

  document
    .querySelector("#proxy-settings-form")
    .addEventListener("submit", async (e) => {
      e.preventDefault();
      const enableProxy = e.target.querySelector("#enableProxy").checked;
      const proxyUrl = e.target.querySelector("#proxyUrl").value;
      const submitButton = e.target.querySelector("button[type=submit]");

      submitButton.setAttribute("disabled", "disabled");

      if (enableProxy) {
        const isValid = await testProxy();
        if (!isValid) {
          submitButton.removeAttribute("disabled");
          return;
        }
      }

      submitButton.classList.add("loader");
      submitButton.innerHTML = "Saving...";

      const body = {
        enableProxy,
        proxyUrl,
      };

      const response = await fetch("/api/v1/settings/proxy", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      })

      const data = await response.json();

      if (!response.ok) {
        butterup.toast({
          message: data.error || "Failed to save settings",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "error",
        });
      } else {
        butterup.toast({
          message: "Proxy settings saved successfully",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "success",
        });

        settings = {
          ...settings,
          enableProxy: body.enableProxy,
          proxyUrl: body.proxyUrl,
        };
      }

      submitButton.removeAttribute("disabled");
      submitButton.classList.remove("loader");
      submitButton.innerHTML = "Save Settings";
    });

  document
    .querySelector("#prowlarr-settings-form")
    .addEventListener("submit", async (e) => {
      e.preventDefault();
      const enableProwlarr = e.target.querySelector("#enableProwlarr").checked;
      const prowlarrHost = e.target.querySelector("#prowlarrHost").value;
      const prowlarrApiKey = e.target.querySelector("#prowlarrApiKey").value;
      const submitButton = e.target.querySelector("button[type=submit]");

      submitButton.setAttribute("disabled", "disabled");

      if (enableProwlarr) {
        const isValid = await testProwlarrConfig();
        if (!isValid) {
          submitButton.removeAttribute("disabled");
          return;
        }
      }

      submitButton.classList.add("loader");
      submitButton.innerHTML = "Saving...";

      const body = {
        enableProwlarr,
        prowlarrHost,
        prowlarrApiKey,
      };

      const response = await fetch("/api/v1/settings/prowlarr", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      })

      const data = await response.json();
      if (!response.ok) {
        butterup.toast({
          message: data.error || "Failed to save settings",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "error",
        });
      } else {
        butterup.toast({
          message: "Prowlarr settings saved successfully",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "success",
        });

        settings = {
          ...settings,
          enableProwlarr: body.enableProwlarr,
          prowlarrHost: body.prowlarrHost,
          prowlarrApiKey: body.prowlarrApiKey,
        };

        // Check if Prowlarr or Jackett is enabled
        if (body?.enableProwlarr || settings?.enableJackett) {
          searchWrapper.classList.remove("hidden");
        } else {
          searchWrapper.classList.add("hidden");
        }
      }

      submitButton.removeAttribute("disabled");
      submitButton.classList.remove("loader");
      submitButton.innerHTML = "Save Settings";
    });

  document
  .querySelector("#jackett-settings-form")
  .addEventListener("submit", async (e) => {
    e.preventDefault();
    const enableJackett = e.target.querySelector("#enableJackett").checked;
    const jackettHost = e.target.querySelector("#jackettHost").value;
    const jackettApiKey = e.target.querySelector("#jackettApiKey").value;
    const submitButton = e.target.querySelector("button[type=submit]");

    submitButton.setAttribute("disabled", "disabled");

    if (enableJackett) {
      const isValid = await testJackettConfig();
      if (!isValid) {
        submitButton.removeAttribute("disabled");
        return;
      }
    }

    submitButton.classList.add("loader");
    submitButton.innerHTML = "Saving...";

    const body = {
      enableJackett,
      jackettHost,
      jackettApiKey,
    };

    const response = await fetch("/api/v1/settings/jackett", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    })

    const data = await response.json();
    if (!response.ok) {
      butterup.toast({
        message: data.error || "Failed to save settings",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
    } else {
      butterup.toast({
        message: "Jackett settings saved successfully",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "success",
      });

      settings = {
        ...settings,
        enableJackett: body.enableJackett,
        jackettHost: body.jackettHost,
        jackettApiKey: body.jackettApiKey,
      };

      // Check if Jackett or Jackett is enabled
      if (body?.enableJackett || settings?.enableJackett) {
        searchWrapper.classList.remove("hidden");
      } else {
        searchWrapper.classList.add("hidden");
      }
    }

    submitButton.removeAttribute("disabled");
    submitButton.classList.remove("loader");
    submitButton.innerHTML = "Save Settings";
  });

  document.querySelector("#torrent_file").addEventListener("change", (e) => {
    const file = e.target.files[0];
    if (file) {
      const formData = new FormData();
      formData.append("torrent", file);

      fetch("/api/v1/torrent/convert", {
        method: "POST",
        body: formData,
      })
        .then(async (res) => {
          if (!res.ok) {
            const err = await res.json();
            throw new Error(err.error || "Failed to upload torrent file");
          }
          return res.json();
        })
        .then((data) => {
          document.querySelector("#magnet").value = data.magnet;
          document
            .querySelector("#torrent-form")
            .dispatchEvent(new Event("submit"));
        })
        .catch((error) => {
          console.error("There was a problem with the fetch operation:", error);
          butterup.toast({
            message: error.message || "Failed to upload torrent file",
            location: "top-right",
            icon: true,
            dismissable: true,
            type: "error",
          });
        });
    }
  });

  const torrentFileWrapper = document.querySelector("#torrent_file_wrapper");
  torrentFileWrapper.addEventListener("dragenter", (e) => {
    e.preventDefault();
    e.stopPropagation();
    torrentFileWrapper.classList.add("drag-over");
  });
  torrentFileWrapper.addEventListener("dragover", (e) => {
    e.preventDefault();
    e.stopPropagation();
  });
  torrentFileWrapper.addEventListener("dragleave", (e) => {
    e.preventDefault();
    e.stopPropagation();
    torrentFileWrapper.classList.remove("drag-over");
  });
  torrentFileWrapper.addEventListener("drop", (e) => {
    e.preventDefault();
    e.stopPropagation();
    torrentFileWrapper.classList.remove("drag-over");
    const files = e.dataTransfer.files;
    if (files.length > 0) {
      const file = files[0];
      if (file.name.endsWith(".torrent")) {
        const formData = new FormData();
        formData.append("torrent", file);

        fetch("/api/v1/torrent/convert", {
          method: "POST",
          body: formData,
        })
          .then(async (res) => {
            if (!res.ok) {
              const err = await res.json();
              throw new Error(err.error || "Failed to upload torrent file");
            }
            return res.json();
          })
          .then((data) => {
            document.querySelector("#magnet").value = data.magnet;
            document
              .querySelector("#torrent-form")
              .dispatchEvent(new Event("submit"));
          })
          .catch((error) => {
            console.error(
              "There was a problem with the fetch operation:",
              error
            );
            butterup.toast({
              message: error.message || "Failed to upload torrent file",
              location: "top-right",
              icon: true,
              dismissable: true,
              type: "error",
            });
          });
      } else {
        butterup.toast({
          message: "Please drop a valid torrent file",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "error",
        });
      }
    }
  });

  // fetch settings
  fetch("/api/v1/settings")
    .then((res) => {
      if (!res.ok) {
        throw new Error("Network response was not ok");
      }
      return res.json();
    })
    .then((data) => {
      settings = data;
      document.querySelector("#enableProxy").checked = data.enableProxy;
      document.querySelector("#proxyUrl").value = data.proxyUrl || "";
      document.querySelector("#enableProwlarr").checked =
        data.enableProwlarr || false;
      document.querySelector("#prowlarrHost").value = data.prowlarrHost || "";
      document.querySelector("#prowlarrApiKey").value =
        data.prowlarrApiKey || "";
      document.querySelector("#enableJackett").checked =
        data.enableJackett || false;
      document.querySelector("#jackettHost").value = data.jackettHost || "";
      document.querySelector("#jackettApiKey").value = data.jackettApiKey || "";

      // Set switch button state
      const switchInputs = document.querySelectorAll("#switchInput");
      switchInputs.forEach((input) => {
        const dot = input.querySelector(".dot");
        const wrapper = input.querySelector(".switch-wrapper");
        if (input.querySelector("input").checked) {
          dot.classList.add("translate-x-full", "!bg-muted");
          wrapper.classList.add("bg-primary");
        } else {
          dot.classList.remove("translate-x-full", "!bg-muted");
          wrapper.classList.remove("bg-primary");
        }
      });

      // Check if Prowlarr or Jackett is enabled
      if (data?.enableProwlarr || data?.enableJackett) {
        searchWrapper.classList.remove("hidden");
      } else {
        searchWrapper.classList.add("hidden");
      }
    })
    .catch((error) => {
      console.error("There was a problem with the fetch operation:", error);
    });
})();
