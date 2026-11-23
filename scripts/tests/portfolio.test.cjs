const {test} = require('node:test');
const assert = require('node:assert/strict');
const {projectPortfolio} = require('../../internal/server/static/portfolio.js');
const {roundedMoney, moneyDirection} = require('../../internal/server/static/portfolio.js');
test('P&L direction matches displayed cents including negative zero and missing values', () => {
 for (const value of [-0, -1e-12, -0.0049, 0.0049]) {
  assert.equal(roundedMoney(value), 0);
  assert.equal(moneyDirection(value), 'neutral');
 }
 assert.equal(roundedMoney(-0.005), -0.01);
 assert.equal(moneyDirection(-0.005), 'loss');
 assert.equal(roundedMoney(1.005), 1.01);
 assert.equal(moneyDirection(0.005), 'gain');
 for (const value of [null, undefined, NaN, Infinity]) assert.equal(moneyDirection(value), 'unavailable');
});
test('zero returns equal starting value plus contributions', ()=>{
 const p=projectPortfolio(1000,100,0,10).at(-1);
 assert.equal(p.value,13000); assert.equal(p.contributed,13000);
});
test('effective annual rate compounds accurately without deposits', ()=>{
 assert.ok(Math.abs(projectPortfolio(1000,0,5,10).at(-1).value-1000*1.05**10)<1e-7);
});
test('negative returns reduce the portfolio and invalid inputs fail', ()=>{
 assert.ok(projectPortfolio(1000,0,-20,2).at(-1).value<1000);
 for (const rate of [-100, NaN, Infinity]) assert.throws(()=>projectPortfolio(1000,0,rate,10));
});
const {holdingReturnReferences}=require('../../internal/server/static/portfolio.js');
test('return context follows holdings and selected currency; unknown funds stay unknown',()=>{
 const holdings=[{symbol:"VWCE",currency:"EUR",quantity:4},{symbol:"OTHER",currency:"EUR",quantity:1},{symbol:"VWRA",currency:"USD",quantity:1}];
 const refs=holdingReturnReferences(holdings,"EUR");
 assert.equal(refs.length,2);
 assert.equal(refs[0].reference.isin,"IE00BK5BQT80");
 assert.equal(refs[0].reference.currency,"USD");
 assert.equal(refs[1].reference,null);
 assert.equal(holdingReturnReferences([], "EUR").length,0);
});
const {annualizedEUR}=require('../../internal/server/static/portfolio.js');
test('FX conversion uses start/end ratio and annualizes the total currency effect',()=>{
 assert.ok(Math.abs(annualizedEUR(10,5,1.2,1.2)-10)<1e-10);
 assert.ok(Math.abs(annualizedEUR(0,1,1.2,1)-20)<1e-10);
 const eur=annualizedEUR(11.56,5,1.1579,1.1355);
 assert.ok(Math.abs((1+eur/100)**5-(1.1156**5)*(1.1579/1.1355))<1e-10);
 assert.throws(()=>annualizedEUR(10,0,1,1));
 assert.throws(()=>annualizedEUR(10,3,1,0));
});
const {fundIssuer, averageMonthlyContribution}=require('../../internal/server/static/portfolio.js');
test('issuer comes from the DEGIRO product name, else a known ticker',()=>{
 assert.equal(fundIssuer({name:"VANGUARD FTSE ALL-WORLD UCITS ETF USD ACC",symbol:"XXXX"}).id,"vanguard");
 assert.equal(fundIssuer({name:"iShares Core MSCI World UCITS ETF"}).id,"ishares");
 assert.equal(fundIssuer({name:"Xtrackers MSCI World"}).logo,false);
 assert.equal(fundIssuer({symbol:"swda"}).id,"ishares");
 assert.equal(fundIssuer({symbol:"VWCE"}).id,"vanguard");
 assert.equal(fundIssuer({name:"APPLE INC",symbol:"AAPL"}),null);
 assert.equal(fundIssuer({}),null);
});
test('monthly contribution averages net buys over the last 12 months',()=>{
 const now=Date.parse("2026-11-16");
 const tx=[
  {type:"BUY",date:"2026-10-01",quantity:10,price:100,currency:"EUR"},
  {type:"BUY",date:"2026-03-01",quantity:20,price:100,currency:"EUR"},
  {type:"SELL",date:"2026-05-01",quantity:-6,price:100,currency:"EUR"},
  {type:"BUY",date:"2026-06-01",quantity:5,price:50,currency:"USD"},
  {type:"BUY",date:"2024-01-02",quantity:99,price:100,currency:"EUR"},
 ];
 const eur=averageMonthlyContribution(tx,"EUR",now);
 assert.deepEqual([eur.months,eur.trades],[12,3]);
 assert.ok(Math.abs(eur.monthly-2400/12)<1e-9);
 assert.equal(averageMonthlyContribution(tx,"GBP",now),null);
 assert.equal(averageMonthlyContribution([],"EUR",now),null);
 // Net sells never produce a negative contribution.
 assert.equal(averageMonthlyContribution([{type:"SELL",date:"2026-10-01",quantity:-1,price:10,currency:"EUR"},{type:"BUY",date:"2024-01-01",quantity:1,price:1,currency:"EUR"}],"EUR",now).monthly,0);
});
test('a history shorter than a year averages over the months it covers',()=>{
 const now=Date.parse("2026-11-16");
 const avg=averageMonthlyContribution([{type:"BUY",date:"2026-08-16",quantity:3,price:100,currency:"EUR"}],"EUR",now);
 assert.equal(avg.months,3);
 assert.ok(Math.abs(avg.monthly-100)<1e-9);
});
const {priceReturnSummary}=require('../../internal/server/static/portfolio.js');
test('price-return summary annualizes only periods of a year or more',()=>{
 const line=priceReturnSummary({periods:[
  {label:"1 year",start_date:"2025-11-14",total:12.5,annualized:12.4},
  {label:"Since first held",start_date:"2026-10-30",total:-1.234,annualized:null},
 ]});
 assert.equal(line,"1 year: 12.40% p.a. · Since 2026-10-30: -1.23% total");
 assert.equal(priceReturnSummary({}),"");
});
