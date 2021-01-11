window.addEventListener("DOMContentLoaded", () => {
  new Zooming({
    bgColor: '#000',
    bgOpacity: '0.9',
    closeOnWindowResize: true,
  }).listen("img");
})