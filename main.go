package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	apiKey     string
	dataFile   string
	purgeLists bool
	verbose    bool
)

type wishListData struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Pieces      []struct {
		ID       string `json:"id"`
		Quantity string `json:"qty"`
		Color    string `json:"color"`
		BOID     string `json:"boid"`
	} `json:"pieces"`
}

//{"wishlist_id":"928017","name":"6971","description":""}
type boWishList struct {
	ID          string `json:"wishlist_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

const (
	dummyListName   = "empty placeholder list"
	boidsCacheFile  = "brickowl-wishlist-boids.json"
	colorsCacheFile = "brickowl-wishlist-colors.json"
)

func init() {

	flag.StringVar(&apiKey, "apikey", "", "api key registered on Brick Owl.")
	flag.StringVar(&dataFile, "datafile", "", "json file used for wishlists.")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose logging output.")
	flag.BoolVar(&purgeLists, "purgelists", false, "Purge existing lists that aren't present in the data file.")
}

func main() {

	flag.Parse()

	//Check mandatory flags
	if apiKey == "" || dataFile == "" {
		flag.Usage()
		os.Exit(1)
		return
	}

	colors, err := getColors()
	if err != nil {
		log.Fatal(err)
	}

	//Get the boids cache
	var boids map[string]string
	err = getLocalData(boidsCacheFile, &boids)
	if os.IsNotExist(err) {
		boids = make(map[string]string)
	} else if err != nil {
		log.Fatal(err)
	}

	//Ensure the boids are saved later
	defer func() {
		err = setLocalData(boidsCacheFile, boids)
		if err != nil {
			log.Fatal(err)
		}
	}()

	var wishListData []wishListData
	if err := getLocalData(dataFile, &wishListData); err != nil {
		log.Fatal(err)
	}

	var boWishLists []boWishList
	if err := getBOData("wishlist/lists", &boWishLists); err != nil {
		log.Fatal(err)
	}

	//We create a dummy list, as brick owl doesn't allow us to delete all lists for a user
	//Creating a dummy means we can delete all other lists if necessary
	dummyFound := false
	for _, boList := range boWishLists {
		if boList.Name == dummyListName {
			dummyFound = true
		}
	}
	if !dummyFound {
		values := url.Values{}
		values.Set("name", dummyListName)
		if err := postBO("wishlist/create_list", values, nil); err != nil {
			log.Fatal(err)
		}
	}

	//First delete any lists from BO that are in our data, or all lists if purge is enabled
	//We delete lists rather than try to update inplace, as this would require more logic and more api requests.
	for _, boList := range boWishLists {
		if boList.Name == dummyListName {
			continue
		}

		found := false
		if !purgeLists {
			for _, wishList := range wishListData {
				if wishList.Name == boList.Name {
					found = true
					break
				}
			}
		}

		if purgeLists || found {
			values := url.Values{}
			values.Set("wishlist_id", boList.ID)
			if err := postBO("wishlist/delete_list", values, nil); err != nil {
				log.Fatal(err)
			}
		}
	}

	//Now process through lists creating items
	for _, list := range wishListData {

		//Create the list
		values := url.Values{}
		values.Set("name", list.Name)
		values.Set("description", list.Description)
		var createResponse struct {
			ID string `json:"wishlist_id"`
		}
		if err := postBO("wishlist/create_list", values, &createResponse); err != nil {
			log.Fatal(err)
		}

		//Loop through pieces
		for _, piece := range list.Pieces {

			//Get the BOID for the piece
			boid := piece.BOID
			if piece.BOID == "" {
				var ok bool
				boid, ok = boids[piece.ID]
				if !ok {
					boid, err = getBOIDForPart(piece.ID)
					if err != nil {
						log.Printf("Error getting BOID for piece with ID '%v' on wish list '%v' - Skipping", piece.ID, list.Name)
						continue
					}
					//Add to the list - it'll be persisted at the end
					boids[piece.ID] = boid
				}
			}

			//Get the color for the piece
			color, ok := colors[strings.ToLower(piece.Color)]
			if !ok {
				log.Printf("Error getting color for piece with ID '%v' on wish list '%v' - Skipping", piece.ID, list.Name)
				continue
			}

			//Create a lot for each piece
			lotValues := url.Values{}
			lotValues.Set("boid", boid)
			lotValues.Set("color_id", color)
			lotValues.Set("wishlist_id", createResponse.ID)
			var createLotResponse struct {
				ID string `json:"lot_id"`
			}
			err = postBO("wishlist/create_lot", lotValues, &createLotResponse)
			if err != nil {
				log.Fatal(err)
			}

			//Then need to update to set the value if its not 1 (cannot do this on create)
			if piece.Quantity != "1" {
				lotValues = url.Values{}
				lotValues.Set("minimum_quantity", piece.Quantity)
				lotValues.Set("wishlist_id", createResponse.ID)
				lotValues.Set("lot_id", createLotResponse.ID)
				err = postBO("wishlist/update", lotValues, nil)
				if err != nil {
					log.Fatal(err)
				}
			}
		}
	}
}

func getWishListData() (wishListData []wishListData, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("Error in getWishListData(): %v", err)
		}
	}()

	//check for saved data
	data, readErr := ioutil.ReadFile(dataFile)
	if readErr != nil {
		log.Printf("Could not read wishlist data from file '%v'", dataFile)
		return
	}

	if err = json.Unmarshal(data, &wishListData); err != nil {
		err = fmt.Errorf("Error parsing wishlist data: %v", err)
		return
	}
	return

}

type boColor struct {
	Name string `json:"name"`
}

func getColors() (map[string]string, error) {

	var err error

	defer func() {
		if err != nil {
			err = fmt.Errorf("Error in getColors(): %v", err)
		}
	}()

	//Check for a cache on disk
	var colorMap map[string]string
	err = getLocalData(colorsCacheFile, &colorMap)
	if err == nil {
		return colorMap, nil
	} else if !os.IsNotExist(err) {
		//Error on checking, doesn't necessarily exist
		return nil, err
	}

	//Continue to get from brick owl api

	//Json format is a map per color id, with details against it in an object. we only need name from the object
	var colorData map[string]boColor
	if err = getBOData("catalog/color_list", &colorData); err != nil {
		return nil, err
	}

	colorMap = make(map[string]string)
	for ID, color := range colorData {
		colorMap[strings.ToLower(color.Name)] = ID
	}

	if err := setLocalData(colorsCacheFile, colorMap); err != nil {
		return nil, err
	}

	return colorMap, nil
}

func getBOIDForPart(partID string) (BOID string, err error) {

	// "https://api.brickowl.com/v1/catalog/id_lookup?key=...&id=3823&type=Part"
	var response struct {
		BOIDs []string `json:"boids"`
	}

	//Match first on ldraw id (which seem to fit bricklink most closely, then use design_id, then all matches)
	urls := []string{
		fmt.Sprintf("catalog/id_lookup?id=%s&type=Part&id_type=ldraw", partID),
		fmt.Sprintf("catalog/id_lookup?id=%s&type=Part&id_type=design_id", partID),
		fmt.Sprintf("catalog/id_lookup?id=%s&type=Part", partID),
	}

	for _, url := range urls {
		err = getBOData(url, &response)
		if err != nil {
			return
		}

		if len(response.BOIDs) > 0 {
			break
		}
	}

	//Check any ids were returned
	if len(response.BOIDs) == 0 {
		err = fmt.Errorf("Failed to lookup any boids for partID '%v'", partID)
		return
	}

	//Need to do some parsing on the response, eg above query returns
	//{"boids":["901078-98","901078-100","901078-95","901078-97","901078-101","901078"]}
	//Initial experimnetation is to take shortest value
	BOID = response.BOIDs[0]
	for _, id := range response.BOIDs[1:] {
		if len(id) < len(BOID) {
			BOID = id
		}
	}

	return
}

func getLocalData(filePath string, data interface{}) error {

	if _, err := os.Stat(filePath); err != nil {
		return err
	}

	//check for saved data
	bytes, readErr := ioutil.ReadFile(filePath)
	if readErr != nil {
		return fmt.Errorf("Could not read data from file '%v'", filePath)
	}

	if err := json.Unmarshal(bytes, data); err != nil {
		return fmt.Errorf("Error parsing data: %v", err)
	}

	return nil
}

func setLocalData(filePath string, data interface{}) error {

	bytes, err := json.Marshal(data)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filePath, bytes, 0644)
	if err != nil {
		return err
	}

	return nil
}

func getBOData(pathAndArgs string, data interface{}) error {

	join := "?"
	if strings.Contains(pathAndArgs, "?") {
		join = "&"
	}

	logVerbose("get request for url '%v'", pathAndArgs)

	url := fmt.Sprintf("https://api.brickowl.com/v1/%s%skey=%s", pathAndArgs, join, apiKey)

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: time.Second * 10,
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("Error requesting data - no response: %v", url)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	logVerbose("response %v for get url '%v': %v...", resp.StatusCode, pathAndArgs, ellipsis(string(body)))

	if err = json.Unmarshal(body, data); err != nil {
		return fmt.Errorf("Error parsing json response: %v", err)
	}

	return nil
}

//Brick owl API POSTS don't use a body - all args are on the querystring
func postBO(pathAndArgs string, data url.Values, response interface{}) error {

	data.Set("key", apiKey)

	logVerbose("post request for url '%v', params: %v", pathAndArgs, data)

	url := fmt.Sprintf("https://api.brickowl.com/v1/%s", pathAndArgs)

	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(data.Encode()))
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("Content-Length", strconv.Itoa(len(data.Encode())))

	client := &http.Client{
		Timeout: time.Second * 10,
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("Error requesting data - no response: %v", url)
	}
	defer resp.Body.Close()

	//Brick owl returns response bodys on certain error code, so attempt to get it.
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	logVerbose("response %v for post url '%v': %v...", resp.StatusCode, url, ellipsis(string(body)))

	if resp.StatusCode != http.StatusOK {
		if len(body) > 0 {
			//Try to get an error message from response
			var errorResponse struct {
				Error struct {
					Status string `json:"status"`
				} `json:"error"`
			}

			if err = json.Unmarshal(body, &errorResponse); err == nil {
				return fmt.Errorf("Error from request '%v', '%v': %v", url, data, errorResponse.Error.Status)
			}
		}
		//Otherwise return the status code
		return fmt.Errorf("Error %v from request `%v'", resp.StatusCode, url)
	}

	//If a response is required then attempt to decode it
	if response != nil {
		if err = json.Unmarshal(body, response); err != nil {
			return fmt.Errorf("Error parsing json response: %v", err)
		}
	}

	return nil
}

func logVerbose(format string, a ...interface{}) {
	if !verbose {
		return
	}

	log.Printf(format, a...)
}

func logReport(format string, a ...interface{}) {
	log.Printf(format, a...)
}

func ellipsis(s string) string {

	const truncateAt = 50

	if len(s) < truncateAt {
		return s
	}

	return s[:50] + "..."
}
